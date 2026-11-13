//go:build basebuild

// The three tests of this tag are dispatch://INFRA-1149's acceptance runs:
// TestTheStoreCensusReadsZeroDifferences in this file, and TestNoWritePathThisChildChangesGetsSlower
// (write_cost_test.go) and TestNoReadThisChildChangesGetsSlowerAndNoEventOutrunsItsNode
// (read_cost_test.go), which share graph_cost_test.go. Each builds the server at the commit this
// branch forked from (baseBuild) and compares it with this build, so each needs a checkout that
// holds origin/main, and each takes from 2 to 45 minutes on an issue pod. They carry a build tag
// of their own and no CI job runs it: they are run on the pod, one at a time and by name, as
//
//	go test -tags basebuild ./cmd/dispatch/ -run '^TestTheStoreCensusReadsZeroDifferences$' -count=1 -v

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
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testtmp"
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
// for it, Author who wrote it, Digest the digest of its text and Parent what holds it. Actor is
// the actor the row records whole, as the row's own JSON, and is empty for a row that records
// none: Author is what the store's made_by is compared with, and Actor is what decides whether
// the row names an author the store holds a node for (graphstore.AuthorOf).
type inventoryRow struct {
	Kind     string `json:"kind"`
	Identity string `json:"identity"`
	Address  string `json:"address"`
	Author   string `json:"author"`
	Digest   string `json:"digest"`
	Parent   string `json:"parent"`
	Actor    string `json:"actor"`
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
// in the second.
//
// The run then asserts that graph_store_cutover reads complete for every kind a table projects,
// and only then holds each projection row to the tuple that answers it, read through the store's
// own current read: a tuple that is missing or that a later one retracted leaves the node row
// standing and takes the link out of what refs.Edges and refs.BacklinkCounts answer, so a
// comparison against graph_nodes alone reads zero differences over a graph that has lost edges.
// The cutover comes first because, until it is complete, those reads answer those kinds from the
// projections and a missing tuple proves nothing either way.
//
// Only then does `envoy-dispatch census --store` run, which is the store-to-projection
// comparison: it must read a population for every kind this child moves (a kind that checked
// zero rows fails this run, by name) and zero differences in both directions, over a database
// holding a deleted block, whose node is gone and whose row is not there. It runs on a session
// whose every transaction is read-only, so a census that wrote a row would fail rather than
// pass. Every clause of the run that counts differences prints what it counted over and fails
// where that is zero (requireADenominator), because a count over an empty set reads zero and
// proves nothing.
//
// Last, the two builds answer the same reads over the same database, field for field, which is a
// different question from store-to-projection equality: both sides can agree while the answer a
// reader gets has changed. Each read must answer something on the older build, since two empty
// answers agree and prove nothing either.
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
	store := storeAgainstInventory(t, ctx, databaseURL, inventory)
	projection := projectionsAgainstInventory(t, ctx, databaseURL, inventory)
	t.Logf("comparison 1, the store against the inventory: %d row(s) the inventory holds and the store lacks, of %d row(s) compared, "+
		"%d author(s), %d row(s) held to the authored_by tuple naming the author they record and %d to having none, "+
		"and %d comment, %d block and %d block reference document(s) among them",
		len(store.missing), store.compared, store.authors, store.authored, store.unauthored,
		store.parents["comment"], store.parents["block"], store.parents["block_ref"])
	requireADenominator(t, "comparison 1, the store against the inventory", store.compared)
	requireADenominator(t, "comparison 1, the author of each row against its node's", store.authors)
	requireADenominator(t, "comparison 1, the authored_by tuple of each row that records an author", store.authored)
	requireADenominator(t, "comparison 1, the absent authored_by tuple of each row that records no author", store.unauthored)
	for _, kind := range []string{"comment", "block", "block_ref"} {
		requireADenominator(t, "comparison 1, the document of each "+kind+" row against its node's", store.parents[kind])
	}
	for _, missing := range store.missing {
		t.Errorf("comparison 1: the store lacks %s %s (%s)", missing.Kind, missing.Identity, missing.Address)
	}
	t.Logf("comparison 2, the projections against the inventory: %d row(s) the inventory holds and the projections lack, of %d row(s) compared",
		len(projection.missing), projection.compared)
	requireADenominator(t, "comparison 2, the projections against the inventory", projection.compared)
	for _, missing := range projection.missing {
		t.Errorf("comparison 2: the projection lacks %s %s (%s)", missing.Kind, missing.Identity, missing.Address)
	}

	// ───── the cutover, asserted before any row is compared with a tuple ─────
	requireTheCutoverIsComplete(t, ctx, databaseURL, censusProjectedKinds)

	// ───── every projection row against the tuple that answers it ─────
	projectionRowsAgainstTheirTuples(t, ctx, databaseURL)

	// ───── only then the store against its projections ─────
	requireTheCensusWritesNothing(t, ctx, databaseURL, func(readOnly string) {
		report := runStoreCensus(t, readOnly)
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
	if _, err := conn.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
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
// out of the repository with `git archive` into a directory of this test's own, so the build is
// the fork point's and not this working copy's. The directory and the binary go when the test
// ends, so every run extracts and builds again, which is about a minute.
func baseBuild(t *testing.T) string {
	t.Helper()
	root := gitOutput(t, ".", "rev-parse", "--show-toplevel")
	commit := gitOutput(t, root, "merge-base", "origin/main", "HEAD")
	if len(commit) < 12 {
		t.Fatalf("git merge-base origin/main HEAD answered %q", commit)
	}
	// No socket is bound anywhere under the tree, so the room asked for is the directory the
	// build runs in rather than the longest path of the archive.
	source := testtmp.Dir(t, filepath.Join("packages", "envoy"))
	archive := exec.Command("git", "-C", root, "archive", commit)
	extract := exec.Command("tar", "-x", "-C", source)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatalf("read the tree of %s: %v", commit, err)
	}
	// The archive's stdout is the pipe tar reads, so its own failure is read from its stderr
	// alone: asking for both, as CombinedOutput does, is `exec: Stdout already set` and the
	// build never runs.
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
	binary := cmdtest.BuildCommand(t, cmdtest.Command{
		Name:   "envoy-dispatch-base",
		Dir:    filepath.Join(source, "packages", "envoy"),
		Target: "./cmd/dispatch",
		Env:    os.Environ(),
	})
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
// are documents of several blocks, a block that cites the other issue and one that cites its
// document, comments by a person and by a session, an ask, and a document of the project's own
// with a comment on it. No write here names a role: the older build records the delivery and
// writes no node, so a role addressed here would be a row the backfill cannot answer for.
func seedThroughTheOlderBuild(t *testing.T, server *dispatchProcess) seededRows {
	t.Helper()
	const project = "CENSUS"
	server.call(t, http.MethodPost, "/api/v1/projects",
		`{"key":"`+project+`","name":"Store census"}`, http.StatusCreated)
	mentioned, _ := createCensusIssue(t, server, project, "The issue the blocks cite",
		"# Cited\n\nThe document every block of the other issue points at.\n")
	// The block that cites the other issue's document is what gives the issue a reference
	// closure (refs.Closure follows links at documents and their blocks alone, never at an
	// issue), so the read of the issue's references answers a member on both builds.
	issue, artifact := createCensusIssue(t, server, project, "The issue the census reads",
		"# Spec\n\nThe first block of this document.\n\n## A nested heading\n\nA block that cites "+
			mentioned+", so the document holds a block reference.\n\nA block that cites dispatch://"+
			mentioned+"/spec, the other issue's document, so the issue's references reach a document.\n\n"+
			"- a list item\n- another item\n")

	seeded := seededRows{project: project, issue: issue, artifact: artifact, mentioned: mentioned}
	seeded.comments = append(seeded.comments,
		server.mention(t, issue, "A comment a person wrote on "+issue+"."),
		server.mention(t, issue, "A second comment, which cites "+mentioned+"."))
	seeded.comments = append(seeded.comments, censusSessionComment(t, server, issue,
		"A comment a session wrote, so the store holds that session."))
	// An open ask, so the reads of the issue's asks, of the project's open asks and of the inbox
	// each answer an ask on both builds rather than two empty lists.
	server.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/asks",
		`{"question":"Does the census read every row?","options":[{"label":"It does"},{"label":"It does not"}]}`,
		http.StatusCreated)
	// Every comment above hangs under the issue, so comments.artifact_id is null on each and the
	// parent the inventory reads for it is empty. A comment on a document of the project's own,
	// which no issue holds, is the one the older build records under that document, so the
	// comparison of a comment's parent has a document to compare.
	notes := censusProjectDocument(t, server, project, "census-notes.md",
		"# Notes\n\nA document of the project's own, which no issue holds.\n")
	seeded.comments = append(seeded.comments, censusDocumentComment(t, server, notes,
		"A comment on the project's own document, which its row records as that document's."))
	return seeded
}

// censusProjectDocument uploads markdown as the document name of project, a document no issue
// holds, and answers its id.
func censusProjectDocument(t *testing.T, server *dispatchProcess, project, name, markdown string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name, "content": markdown})
	if err != nil {
		t.Fatalf("encode the project's document: %v", err)
	}
	answer := server.send(t, http.MethodPost, "/api/v1/projects/"+project+"/artifacts", string(body), asUser(dispatchTestLogin))
	if answer.status != http.StatusCreated && answer.status != http.StatusOK {
		t.Fatalf("upload %s to project %s: %s", name, project, answer)
	}
	var uploaded struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(answer.body, &uploaded); err != nil || uploaded.Artifact.ID == "" {
		t.Fatalf("read the upload of %s back: %v: %s", name, err, answer.body)
	}
	return uploaded.Artifact.ID
}

// censusDocumentComment writes body as a comment on the document no issue holds, as the test's
// person, and answers its id.
func censusDocumentComment(t *testing.T, server *dispatchProcess, document, body string) string {
	t.Helper()
	input, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		t.Fatalf("encode the document's comment: %v", err)
	}
	created := server.call(t, http.MethodPost, "/api/v1/artifacts/"+document+"/comments", string(input), http.StatusCreated)
	var comment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &comment); err != nil || comment.ID == "" {
		t.Fatalf("read the document's comment back: %v: %s", err, created)
	}
	return comment.ID
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
// block's is the text digest its row already holds, and a block reference's is its far end. A
// comment records the actor that wrote it; document_blocks and block_refs have no column for
// one, so neither kind records an author.
func readInventory(t *testing.T, ctx context.Context, databaseURL string) []inventoryRow {
	t.Helper()
	conn := censusConnect(t, ctx, databaseURL)
	rows, err := conn.Query(ctx, `
		select 'comment' as kind, c.id::text as identity, c.id::text as address,
		       coalesce(c.author->>'kind', '') || ':' ||
		       coalesce(c.author->>'id', c.author->>'owner', '') as author,
		       md5(c.body) as digest, coalesce(c.artifact_id::text, '') as parent,
		       coalesce(c.author::text, '') as actor
		from comments c
		union all
		select 'block', d.artifact_id::text || '#' || d.block_id, d.node_id, '',
		       d.text_sha256, d.artifact_id::text, ''
		from document_blocks d
		union all
		select 'block_ref',
		       r.artifact_id::text || '#' || r.block_id || ' ' || r.kind || ' -> ' || r.to_kind || ':' || r.to_id,
		       r.from_node, '', md5(r.to_kind || ':' || r.to_id), r.artifact_id::text, ''
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
		if err := rows.Scan(&row.Kind, &row.Identity, &row.Address, &row.Author, &row.Digest, &row.Parent, &row.Actor); err != nil {
			t.Fatalf("scan an inventory row: %v", err)
		}
		inventory = append(inventory, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the inventory: %v", err)
	}
	return inventory
}

// keepInventory writes the inventory to a file in a directory of this test's own and says where
// it is. The directory goes when the test ends, so a run that fails prints the inventory too, for
// a person to read it against the comparisons below.
func keepInventory(t *testing.T, inventory []inventoryRow) {
	t.Helper()
	encoded, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		t.Fatalf("encode the inventory: %v", err)
	}
	const name = "store-census-inventory.json"
	file := filepath.Join(testtmp.Dir(t, name), name)
	if err := os.WriteFile(file, append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("keep the inventory: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the inventory of what the older build wrote:\n%s", encoded)
		}
	})
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

// censusComparison is what one comparison of this run read: how many rows it held against the
// other side, how many of those carried an author and a document to hold as well, and the rows
// the inventory holds and that side lacks. The counts are the denominators the run prints beside
// what each comparison found, because a comparison that held no row finds no difference and
// proves nothing.
//
// parents counts by kind the rows that name a document, so each kind has a document of its own
// to compare: a kind whose rows all name none would compare an empty parent with an empty one and
// pass. authored counts the rows that record an author the store can name and are held to the
// authored_by tuple naming that author, and unauthored the rows that record none and are held to
// having no authored_by tuple at all.
type censusComparison struct {
	compared, authors    int
	parents              map[string]int
	authored, unauthored int
	missing              []inventoryRow
}

// requireADenominator fails the run where a clause that counts differences counted over nothing.
// An anti-join over an empty set reads no difference and passes while proving nothing, which is
// the vacuous pass this check forbids, so every clause of this file that counts prints what it
// counted over and stops the run when that is zero.
func requireADenominator(t *testing.T, clause string, denominator int) {
	t.Helper()
	if denominator == 0 {
		t.Fatalf("%s compared zero rows, so the zero differences it read prove nothing", clause)
	}
}

// storedNode is what the store says of one node, as this comparison reads it: who made it, who
// its current authored_by tuples name, and the document it is in, each read out of the store
// alone.
type storedNode struct {
	state, author, authoredBy, parent string
}

// storeAgainstInventory holds the store to the inventory, row by row: every comment and every
// block of the inventory has a node of its own kind, the comment's node names the author the
// row recorded, its authored_by tuple names that author exactly where the row records one, and
// the node's own tuple reaches the document the row belongs to. It answers what it compared and
// the rows the inventory holds and the store lacks, and reports a node that holds a different
// author or a different document as a difference of its own.
//
// The parent is read the way a current read reads it: the newest in_document tuple of a block
// and the newest owned_by of a comment, each kept while it asserts, resolved to the artifact the
// address names. The authored_by tuples are read the same way: the newest tuple at each far end,
// kept while it asserts.
func storeAgainstInventory(t *testing.T, ctx context.Context, databaseURL string, inventory []inventoryRow) censusComparison {
	t.Helper()
	conn := censusConnect(t, ctx, databaseURL)
	rows, err := conn.Query(ctx, `
		select n.kind, n.id, n.state,
		       coalesce(n.made_by->>'kind', '') || ':' ||
		       coalesce(n.made_by->>'id', n.made_by->>'owner', '') as author,
		       coalesce((
		           select string_agg(current.to_kind || ':' || current.to_id, ',' order by current.to_kind, current.to_id)
		           from (
		               select distinct on (written.to_kind, written.to_id) written.to_kind, written.to_id, written.asserted
		               from graph_tuples written
		               where written.from_kind = n.kind and written.from_id = n.id and written.kind = $1
		               order by written.to_kind, written.to_id, written.seq desc
		           ) current
		           where current.asserted
		       ), '') as authored_by,
		       coalesce((
		           select a.id::text from artifacts a where a.ref_key = (
		               select held.to_id from graph_tuples held
		               where held.from_kind = n.kind and held.from_id = n.id
		                 and held.kind in ('in_document', 'owned_by') and held.to_kind = 'artifact'
		               order by held.seq desc limit 1
		           )
		       ), '') as parent
		from graph_nodes n where n.kind in ('comment', 'block')
	`, graphstore.TupleAuthoredBy)
	if err != nil {
		t.Fatalf("read the nodes of the store: %v", err)
	}
	defer rows.Close()
	held := map[string]storedNode{}
	for rows.Next() {
		var kind, id string
		var node storedNode
		if err := rows.Scan(&kind, &id, &node.state, &node.author, &node.authoredBy, &node.parent); err != nil {
			t.Fatalf("scan a node: %v", err)
		}
		held[kind+"\x00"+id] = node
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the nodes of the store: %v", err)
	}
	read := censusComparison{parents: map[string]int{}}
	for _, row := range inventory {
		nodeKind := "comment"
		if row.Kind != "comment" {
			nodeKind = "block"
		}
		node, found := held[nodeKind+"\x00"+row.Address]
		if !found {
			read.compared++
			read.missing = append(read.missing, row)
			continue
		}
		read.compared++
		// A block's row records no author: document_blocks has no column for one, and neither
		// has block_refs. The backfill is the writer of the node and the tuples it states for
		// such a row, recorded as the system actor in made_by and in each tuple's who, and it
		// states no authored_by for it, since naming the system actor, or anyone, as the block's
		// author would state a fact no row holds (graphcensus's backfillActor). So made_by is
		// held to the row's author only where the row records one, and the authored_by tuple is
		// held both ways: to the author a row records, and to being absent where it records none.
		if row.Author != "" {
			read.authors++
			if node.author != row.Author {
				t.Errorf("comparison 1: the node of %s %s was made by %s, want the author the row recorded, %s",
					row.Kind, row.Identity, node.author, row.Author)
			}
		}
		if author := recordedAuthor(t, row); author != "" {
			read.authored++
			if node.authoredBy != author {
				t.Errorf("comparison 1: the node of %s %s is authored_by %q, want the one author the row records, %s",
					row.Kind, row.Identity, node.authoredBy, author)
			}
		} else {
			read.unauthored++
			if node.authoredBy != "" {
				t.Errorf("comparison 1: the node of %s %s is authored_by %s, but the row records no author: the backfill writes that node's tuples as the system actor and names no author for it",
					row.Kind, row.Identity, node.authoredBy)
			}
		}
		// A block reference's row names its block, whose document is the one the reference's own
		// row belongs to, so both kinds are held to the same parent. A row the older build wrote
		// under no document states nothing to compare, so it is no denominator of this clause.
		if row.Parent != "" {
			read.parents[row.Kind]++
		}
		if node.parent != row.Parent {
			t.Errorf("comparison 1: the node of %s %s is in document %q, want the row's own %q",
				row.Kind, row.Identity, node.parent, row.Parent)
		}
	}
	return read
}

// recordedAuthor is the node the store must name as the author of an inventory row, as
// `<kind>:<id>`: the one the row's recorded actor names (graphstore.AuthorOf, the one rule the
// writers and the readers of this store share), and empty for a row that records no actor or
// one that names nobody, such as the deployment's shared bearer.
func recordedAuthor(t *testing.T, row inventoryRow) string {
	t.Helper()
	if row.Actor == "" {
		return ""
	}
	var actor model.Actor
	if err := json.Unmarshal([]byte(row.Actor), &actor); err != nil {
		t.Fatalf("read the actor %s %s records: %v", row.Kind, row.Identity, err)
	}
	kind, id, named := graphstore.AuthorOf(actor)
	if !named {
		return ""
	}
	return kind + ":" + id
}

// projectionsAgainstInventory holds each projection to the inventory the same way: every row the
// older build wrote is still in its own table, with the same identity, the same author, the same
// text digest and the same parent. A row the migration or the backfill dropped from both sides
// shows here and never in the census. Every row of the inventory is expected back: what this
// build deletes later, it deletes from a document of its own, which the inventory never held.
func projectionsAgainstInventory(t *testing.T, ctx context.Context, databaseURL string, inventory []inventoryRow) censusComparison {
	t.Helper()
	held := map[string]inventoryRow{}
	for _, row := range readInventory(t, ctx, databaseURL) {
		held[row.Kind+"\x00"+row.Identity] = row
	}
	read := censusComparison{}
	for _, row := range inventory {
		reads, found := held[row.Kind+"\x00"+row.Identity]
		read.compared++
		switch {
		case !found:
			read.missing = append(read.missing, row)
		case reads != row:
			t.Errorf("comparison 2: %s %s reads back as %+v, want the inventory's %+v",
				row.Kind, row.Identity, reads, row)
		}
	}
	return read
}

// ───── the cutover, and each row against the tuple that answers it ────────

// censusProjectedKinds are the kinds a table projects, which are the kinds graph_store_cutover
// names: the kinds whose rows this run holds to the tuples that answer them. The list is the
// test's own, as censusKinds is, so a kind the schema stops naming is read by a person before
// this test agrees with it.
var censusProjectedKinds = []string{"block", "block_ref", "comment"}

// The two tuple kinds a comment states about the documents it hangs under, as the writer spells
// them (api/comment_create.go, and graphcensus's backfill for a comment the older build wrote).
// A block's are refs.TupleInDocument and graphstore.TuplePartOf, a block reference's is the
// row's own kind, and the author of any of them is graphstore.TupleAuthoredBy.
const (
	censusTupleAnchoredTo = "anchored_to"
	censusTupleOwnedBy    = "owned_by"
)

// requireTheCutoverIsComplete fails the run unless graph_store_cutover reads complete for every
// kind the comparison below holds to the tuples, naming the kind that does not and stopping
// before that comparison runs.
//
// The order is the point. While a kind's row reads false, graph_edges answers that kind from its
// own projection and refs.BacklinkCounts counts it there, so a tuple that is missing or retracted
// changes no answer a reader gets and a comparison made then proves nothing about the tuples.
// Complete is the one state in which the tuples are what the graph answers from.
func requireTheCutoverIsComplete(t *testing.T, ctx context.Context, databaseURL string, kinds []string) {
	t.Helper()
	conn := censusConnect(t, ctx, databaseURL)
	rows, err := conn.Query(ctx, `select kind, complete from graph_store_cutover order by kind`)
	if err != nil {
		t.Fatalf("read the cutover: %v", err)
	}
	defer rows.Close()
	cutover := map[string]bool{}
	var read []string
	for rows.Next() {
		var kind string
		var complete bool
		if err := rows.Scan(&kind, &complete); err != nil {
			t.Fatalf("scan a cutover row: %v", err)
		}
		cutover[kind] = complete
		read = append(read, kind+"="+strconv.FormatBool(complete))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the cutover: %v", err)
	}
	t.Logf("graph_store_cutover holds %d row(s): %s", len(read), strings.Join(read, " "))
	requireADenominator(t, "the cutover of the kinds this run compares", len(read))
	for _, kind := range kinds {
		complete, found := cutover[kind]
		switch {
		case !found:
			t.Fatalf("graph_store_cutover holds no row for kind %s, so nothing says the store answers that kind", kind)
		case !complete:
			t.Fatalf("the cutover of kind %s is incomplete: graph_edges still answers that kind from its projection, so holding its rows to the tuples would prove nothing",
				kind)
		}
	}
	t.Logf("the cutover is complete for %s, which is the state the graph answers those kinds from the tuples in",
		strings.Join(kinds, ", "))
}

// projectedTuple is one fact a projection row states, as the store must hold it: the kind of row
// it came from, the row as a failure names it, the node that row names, and the tuple that
// answers the row.
type projectedTuple struct {
	kind             string
	row              string
	nodeKind, nodeID string
	tupleKind        string
	toKind, toID     string
}

// projectionRowsAgainstTheirTuples holds every projection row to the tuple that answers it, and
// not to graph_nodes alone: a comment's owner, its anchor and its author, a block's document and
// the block or document that holds it, and every link a block reference records. Each is read
// back through the store's own current read (graphstore.CurrentOut), which answers the newest
// tuple per far end while that newest one asserts, so a tuple that was never written and a tuple
// a later one retracted are both absent here.
//
// This is the comparison the node rows cannot make. A missing or retracted tuple leaves every
// node row standing, so a census that reads the rows alone reports zero differences over a graph
// that has lost edges: refs.Edges and refs.BacklinkCounts answer those kinds from the tuples
// once the cutover is complete, which requireTheCutoverIsComplete asserts before this runs.
//
// A comment whose author is a session is held here too, by the authored_by tuple that reaches
// that session's node: a session that wrote a comment and has no node fails this comparison.
//
// Each kind prints how many links it compared, which is the denominator of its count of rows
// with no tuple, and a kind that compared none fails the run.
func projectionRowsAgainstTheirTuples(t *testing.T, ctx context.Context, databaseURL string) {
	t.Helper()
	conn := censusConnect(t, ctx, databaseURL)
	wanted := censusCommentTuples(t, ctx, conn)
	wanted = append(wanted, censusBlockTuples(t, ctx, conn)...)
	wanted = append(wanted, censusBlockRefTuples(t, ctx, conn)...)
	held := map[[2]string][]graphstore.Tuple{}
	compared := map[string]int{}
	absent := map[string]int{}
	for _, want := range wanted {
		node := [2]string{want.nodeKind, want.nodeID}
		current, found := held[node]
		if !found {
			answered, err := graphstore.CurrentOut(ctx, conn, want.nodeKind, want.nodeID, nil)
			if err != nil {
				t.Fatalf("read the tuples out of %s %s: %v", want.nodeKind, want.nodeID, err)
			}
			held[node] = answered
			current = answered
		}
		compared[want.kind]++
		if slices.ContainsFunc(current, func(tuple graphstore.Tuple) bool {
			return tuple.Kind == want.tupleKind && tuple.ToKind == want.toKind && tuple.ToID == want.toID
		}) {
			continue
		}
		absent[want.kind]++
		t.Errorf("the store states no %s tuple from %s %s to %s %s, which is the tuple that answers %s: its node row stands, and the link is gone from what refs.Edges and refs.BacklinkCounts answer",
			want.tupleKind, want.nodeKind, want.nodeID, want.toKind, want.toID, want.row)
	}
	for _, kind := range censusProjectedKinds {
		t.Logf("the %s rows against the tuples that answer them: %d link(s) compared, %d of them stated by no tuple",
			kind, compared[kind], absent[kind])
		requireADenominator(t, "the "+kind+" rows against the tuples that answer them", compared[kind])
	}
}

// censusCommentTuples is what every comment row states: the document that owns it, the document
// it anchors to where it has one, and its author where the row's actor names one, read through
// graphstore.AuthorOf, which is the one rule the writers and the readers of this store share.
func censusCommentTuples(t *testing.T, ctx context.Context, conn *pgx.Conn) []projectedTuple {
	t.Helper()
	rows, err := conn.Query(ctx, `
		select c.id::text, c.author, coalesce(owner.ref_key, ''), coalesce(anchor.ref_key, '')
		from comments c
		left join artifacts owner on owner.id = c.artifact_id
		left join artifacts anchor on anchor.id = (c.anchor->>'artifact_id')::uuid
		order by c.id
	`)
	if err != nil {
		t.Fatalf("read what the comment rows state: %v", err)
	}
	defer rows.Close()
	var wanted []projectedTuple
	for rows.Next() {
		var id, owner, anchor string
		var author []byte
		if err := rows.Scan(&id, &author, &owner, &anchor); err != nil {
			t.Fatalf("scan a comment row: %v", err)
		}
		states := func(kind, toKind, toID string) {
			wanted = append(wanted, projectedTuple{
				kind: "comment", row: "comment " + id, nodeKind: "comment", nodeID: id,
				tupleKind: kind, toKind: toKind, toID: toID,
			})
		}
		if owner != "" {
			states(censusTupleOwnedBy, "artifact", owner)
		}
		if anchor != "" {
			states(censusTupleAnchoredTo, "artifact", anchor)
		}
		var actor model.Actor
		if err := json.Unmarshal(author, &actor); err != nil {
			t.Fatalf("read the author of comment %s: %v", id, err)
		}
		// A comment written on the deployment's shared bearer names nobody, and the store holds
		// no node for nobody, so such a row states no authored_by to hold it to.
		if authorKind, authorID, named := graphstore.AuthorOf(actor); named {
			states(graphstore.TupleAuthoredBy, authorKind, authorID)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read what the comment rows state: %v", err)
	}
	return wanted
}

// censusBlockTuples is what every block row states: the document it is a block of, and the block
// or the document that holds it, which document_blocks spells as parent_block_id. A block's node
// address is its document's ref_key, '#' and its block id, as the insert of document_blocks
// builds it, so the parent's address is built here the same way.
func censusBlockTuples(t *testing.T, ctx context.Context, conn *pgx.Conn) []projectedTuple {
	t.Helper()
	rows, err := conn.Query(ctx, `
		select d.node_kind, d.node_address, a.ref_key,
		       case when d.parent_block_id is null then '' else a.ref_key || '#' || d.parent_block_id end
		from document_blocks d join artifacts a on a.id = d.artifact_id
		where d.node_kind is not null and d.node_address is not null
		order by d.node_address
	`)
	if err != nil {
		t.Fatalf("read what the block rows state: %v", err)
	}
	defer rows.Close()
	var wanted []projectedTuple
	for rows.Next() {
		var nodeKind, address, document, parent string
		if err := rows.Scan(&nodeKind, &address, &document, &parent); err != nil {
			t.Fatalf("scan a block row: %v", err)
		}
		states := func(kind, toKind, toID string) {
			wanted = append(wanted, projectedTuple{
				kind: "block", row: "block " + address, nodeKind: nodeKind, nodeID: address,
				tupleKind: kind, toKind: toKind, toID: toID,
			})
		}
		states(refs.TupleInDocument, "artifact", document)
		if parent == "" {
			states(graphstore.TuplePartOf, "artifact", document)
			continue
		}
		states(graphstore.TuplePartOf, refs.BlockNodeKind, parent)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read what the block rows state: %v", err)
	}
	return wanted
}

// censusBlockRefTuples is what every block reference states: one link out of the block whose node
// the row names, under the row's own kind and at the row's own far end. A row naming no node is a
// block past the store's address bound (refs.MaxBlockNodeBytes), which the store holds nothing
// about by design, so this leaves those rows out as the census does.
func censusBlockRefTuples(t *testing.T, ctx context.Context, conn *pgx.Conn) []projectedTuple {
	t.Helper()
	rows, err := conn.Query(ctx, `
		select r.node_kind, r.node_address, r.kind, r.to_kind, r.to_id
		from block_refs r
		where r.node_kind is not null and r.node_address is not null
		order by r.node_address, r.kind, r.to_kind, r.to_id
	`)
	if err != nil {
		t.Fatalf("read what the block reference rows state: %v", err)
	}
	defer rows.Close()
	var wanted []projectedTuple
	for rows.Next() {
		var nodeKind, address, kind, toKind, toID string
		if err := rows.Scan(&nodeKind, &address, &kind, &toKind, &toID); err != nil {
			t.Fatalf("scan a block reference row: %v", err)
		}
		wanted = append(wanted, projectedTuple{
			kind: "block_ref", row: "block_ref " + address + " " + kind + " -> " + toKind + ":" + toID,
			nodeKind: nodeKind, nodeID: address, tupleKind: kind, toKind: toKind, toID: toID,
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read what the block reference rows state: %v", err)
	}
	return wanted
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

// requireTheCensusWritesNothing runs the census on a session that cannot write and proves it
// wrote nothing. census is given a URL every transaction of whose session is read-only
// (readOnlySession), so a statement of the census that inserted, updated or deleted a row,
// inside its snapshot or outside it, fails, the census exits 2, and runStoreCensus fails the
// run. The store's marks - the log's highest sequence and both tables' row counts - are read
// before and after it as well.
func requireTheCensusWritesNothing(t *testing.T, ctx context.Context, databaseURL string, census func(readOnly string)) {
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
	readOnly := readOnlySession(t, ctx, databaseURL)
	before := read()
	census(readOnly)
	if after := read(); after != before {
		t.Errorf("the census moved the store: highest sequence, tuples and nodes were %v and are %v", before, after)
	}
}

// readOnlySession answers databaseURL with every transaction of the sessions it opens read-only:
// the startup parameter `options` carries `-c default_transaction_read_only=on`, which Postgres
// applies to the session, and pgx sends every key of the URL it does not read itself as such a
// parameter (pgconn.ParseConfig's RuntimeParams). The URL is proved before it is answered: a
// session opened with it must read the setting on and have an update refused as
// read_only_sql_transaction (25006). A URL that lost the option would otherwise hand the census
// a session that writes, and a census that wrote would pass.
func readOnlySession(t *testing.T, ctx context.Context, databaseURL string) string {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("read the census database URL: %v", err)
	}
	query := parsed.Query()
	query.Set("options", "-c default_transaction_read_only=on")
	// Encode writes a space as `+`, which pgconn hands to the server as it is, and Postgres then
	// reads the option as one named `+default_transaction_read_only` and refuses the connection.
	// A literal `+` is %2B once encoded, so every `+` left is a space and is spelt %20.
	parsed.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20")
	readOnly := parsed.String()
	conn := censusConnect(t, ctx, readOnly)
	var setting string
	if err := conn.QueryRow(ctx, `show default_transaction_read_only`).Scan(&setting); err != nil || setting != "on" {
		t.Fatalf("a session opened with the census's URL reads default_transaction_read_only %q (%v), want on", setting, err)
	}
	_, err = conn.Exec(ctx, `update graph_store_cutover set complete = complete`)
	var postgres *pgconn.PgError
	if !errors.As(err, &postgres) || postgres.Code != "25006" {
		t.Fatalf("a session opened with the census's URL took an update (%v), want it refused as read_only_sql_transaction (25006)", err)
	}
	t.Logf("the census runs on a session whose every transaction is read-only: an update there is refused (%s)", postgres.Message)
	return readOnly
}

// ───── the old read beside the new read ───────────────────────────────────

// compareTheOldReadWithTheNew runs the older build's answer beside this build's, over the same
// database, for each read this child touched, and compares the two field for field. Store-to-
// projection equality says nothing about this: both sides can agree and the answer a reader gets
// still change.
//
// A read counts as compared only where the older build's answer holds something
// (answerElements): two empty lists agree field for field and prove nothing, so each read that
// answers nothing fails the run by name, and the denominator printed is the reads that compared
// an answer.
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
	// own comment read is the one here. lists names the fields of an object answer that hold
	// what the read lists; a read that names none lists its own items, or is one object.
	reads := []struct {
		path  string
		lists []string
	}{
		{path: "/api/v1/issues/" + seeded.issue},
		{path: "/api/v1/issues/" + seeded.issue + "/comments"},
		{path: "/api/v1/issues/" + seeded.issue + "/references", lists: []string{"members"}},
		{path: "/api/v1/issues/" + seeded.issue + "/asks"},
		{path: "/api/v1/issues?project=" + seeded.project},
		{path: "/api/v1/artifacts/" + seeded.artifact},
		{path: "/api/v1/artifacts/" + seeded.artifact + "/text", lists: []string{"markdown"}},
		{path: "/api/v1/artifacts/" + seeded.artifact + "/blocks"},
		{path: "/api/v1/artifacts/" + seeded.artifact + "/references", lists: []string{"outgoing", "referenced_by"}},
		{path: "/api/v1/projects/" + seeded.project + "/artifacts"},
		{path: "/api/v1/asks/open?project=" + seeded.project, lists: []string{"asks"}},
		{path: "/api/v1/inbox"},
	}
	compared, differing := 0, 0
	for _, read := range reads {
		was := decodeRead(t, before.call(t, http.MethodGet, read.path, "", http.StatusOK))
		is := decodeRead(t, now.call(t, http.MethodGet, read.path, "", http.StatusOK))
		elements := answerElements(was, read.lists)
		if elements == 0 {
			t.Errorf("GET %s answers nothing on the older build, so comparing it with this build's answer compares nothing: the seed must write what it lists",
				read.path)
		} else {
			compared++
		}
		if !reflect.DeepEqual(was, is) {
			differing++
			t.Errorf("GET %s answers differently on the two builds:\nthe older build %s\nthis build     %s",
				read.path, mustEncode(t, was), mustEncode(t, is))
			continue
		}
		t.Logf("GET %s answers field for field on both builds, over %d element(s)", read.path, elements)
	}
	t.Logf("the old read beside the new: %d of %d read(s) compared an answer that holds something, %d of the %d answering differently",
		compared, len(reads), differing, len(reads))
	requireADenominator(t, "the old read beside the new read", compared)
}

// answerElements counts what one read's answer holds: the items of a list answer, and an object
// answer as one; where the read names the fields it lists, the items of those fields alone, a
// text counting as one where it is not empty.
func answerElements(answer any, lists []string) int {
	if len(lists) == 0 {
		return elementsOf(answer)
	}
	object, _ := answer.(map[string]any)
	elements := 0
	for _, name := range lists {
		elements += elementsOf(object[name])
	}
	return elements
}

func elementsOf(value any) int {
	switch held := value.(type) {
	case []any:
		return len(held)
	case map[string]any:
		if len(held) > 0 {
			return 1
		}
	case string:
		if held != "" {
			return 1
		}
	}
	return 0
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
