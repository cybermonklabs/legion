//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go. This file's test builds two
// earlier builds with baseBuildOf, main's, the commit MAIN_BASELINE_SHA names, and the one
// BASELINE_SHA names, and is run as
//
//	BASELINE_SHA=<the commit of cybermonklabs/legion#4173 this branch holds> MAIN_BASELINE_SHA=$(git merge-base origin/main HEAD) go test -tags basebuild -timeout 30m ./cmd/dispatch/ -run '^TestALensIsStaleOnceAnotherRowStandsAtItsDocumentsAddress$' -count=1 -v

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/testbaseline"
	"github.com/sjawhar/envoy/testpool"
)

const (
	// staleLensSigningKey is the one DISPATCH_SIGNING_KEY every process of the run carries: a
	// process whose key a live peer does not share refuses to start (processes.Register).
	staleLensSigningKey = "stale-lens-signing-key"
	// staleLensProject is the project of the spec's issue, on the run's own database.
	staleLensProject = "STALE"
	// staleLensSecond is the second person, who invokes LP on FO reserved for himself. The person
	// is dispatchTestLogin; both are signed in as the processes trust, by X-Dispatch-User (asUser).
	staleLensSecond = "bob"
	// staleLensKind is the lens_kind every lens of the run is of.
	staleLensKind = "trade-off"
)

// A lens is stale once another row stands at its document's address, whatever build removed the
// document and whatever build made the row that stands there now (spec requirement r-proposed,
// c-settle).
//
// Two earlier builds and this one serve one database, as a roll and a rollback serve them: main's,
// the commit MAIN_BASELINE_SHA names, whose removal and create of a document write no node, and
// cybermonklabs/legion#4173's, the commit BASELINE_SHA names, whose removal states the document's
// node gone and nothing more and whose create writes the document's node (queueNewArtifact). The
// test refuses a BASELINE_SHA whose migrations lack 20261109013400 (graph_store) and a
// MAIN_BASELINE_SHA whose migrations hold it. Each earlier build is built by baseBuildOf and run
// through startDispatchProcessWith with dispatchOptions.Binary, as
// TestTheStoreCensusReadsZeroDifferences runs its older build. The earlier builds boot on the
// empty database first, in the order of their releases, and this build after them, so every build
// serves on the schema this build migrates.
//
// In each case the person makes a document A of the spec's issue by typing a title through this
// build's route, invokes a trade-off analysis LO on A with one finding FO, and the second person
// invokes a lens LP on FO, reserved for himself (staleLensRoll.lensedDocument); LO records A's row.
// The test then holds the block-index lease and the reference forms drain's, as costHoldBlockIndex
// holds them, so no tick of any build and no pass of this build's forms drain runs from before the
// earlier build's removal until the reads that must come before this build's tick, and an earlier
// build removes A and makes a document B of the same title, which takes A's address. With the
// leases held, this build answers LO, FO and LP as gone nodes (requireAnsweredAsGone). Before it
// waits for this build's tick, the test stops every earlier build's process, a SIGTERM and a wait
// for it to exit (stopProcess), because a period an earlier build's process serves is a period
// this build's tick waits out; then it gives the block-index lease back alone, and once this
// build's tick has run on B's artifact.created event (awaitThisBuildsTick), LO, FO and LP each
// read gone in the store. It keeps the forms drain's lease until the case's reads after the tick
// are done (blockIndexHold.giveBackIndex), so what they read is the tick's work: the drain indexes
// a document through the block index's own index, and B, which an earlier build wrote, can be owed
// to it. It starts the earlier builds again, on the same database, for the next case.
//
//   - First, main's server removes A and makes B, so A's live node stands behind B's row. With the
//     leases still held the person invokes LB on B's address through this build, which answers 201,
//     and LB records B's row, the row the artifacts table holds at the address, and not A's. After
//     this build's tick LB reads live with its on naming B's address, and still so after this
//     build's backfill, envoy-dispatch backfill-graph-store, and after B is retired through
//     docs.RetireDocument, at B's last version, @v1.
//   - Second, main's server removes A, and cybermonklabs/legion#4173's server makes B, which
//     writes the document's node for B.
//   - Third, cybermonklabs/legion#4173's server removes A, which states A's node gone, and main's
//     server makes B, which writes no node, so B's row stands behind A's gone node. With the leases
//     held the person's lens write on B's address answers 409 NODE_GONE; once this build's tick
//     has run its object pass on B's artifact.created event, B's node reads live and the same
//     write answers 201.
func TestALensIsStaleOnceAnotherRowStandsAtItsDocumentsAddress(t *testing.T) {
	ctx := context.Background()
	roll := newStaleLensRoll(t)

	// ───── the first case: main removes A and makes B ─────
	first := roll.lensedDocument(t, "the first case", "Made again by main.md")
	hold := roll.holdTheLease(t)
	removeTypedDocument(t, roll.main.process, roll.main.name, first.a)
	roll.makeB(t, first, roll.main)
	roll.requireDocumentNode(t, first, graphstore.StateLive, "main's removal and create write no node, so A's live node stands behind B's row")
	roll.requireStatedByBsCreate(t, first, false, "main's create writes no node")
	written := roll.sendLens(t, dispatchTestLogin, map[string]any{"on": first.address, "kind": staleLensKind})
	if written.status != http.StatusCreated {
		t.Fatalf("the person's lens write LB on %s answers %s, want 201", first.address, written)
	}
	lb := staleLensOf(decodeStaleLens(t, "LB", written))
	if got := roll.recordedRow(t, lb.id); got != first.b.id {
		t.Errorf("the lens %s on %s records the row %s, want %s, the row at the address when it was written",
			lb.address, first.address, staleLensRowWords(got), first.b.id)
	}
	roll.requireAnsweredAsGone(t, first, &lb)
	roll.stopEarlierBuilds(t)
	hold.giveBackIndex()
	event := roll.awaitThisBuildsTick(t, first)
	roll.requireGoneAfterTheTick(t, first, event)
	roll.requireLensLive(t, first, lb, "this build's tick", first.address)
	hold.giveBack()

	code, out := runDispatchCommand(t, roll.head, roll.databaseURL, "backfill-graph-store")
	t.Logf("backfill-graph-store:\n%s", out)
	if code != 0 {
		t.Fatalf("backfill-graph-store exited %d", code)
	}
	roll.requireLensLive(t, first, lb, "the backfill", first.address)

	if err := pgx.BeginFunc(ctx, roll.pool, func(tx pgx.Tx) error {
		_, err := docs.RetireDocument(ctx, tx, first.b.id, model.Actor{Kind: "user", ID: dispatchTestLogin}, "The test retires B.")
		return err
	}); err != nil {
		t.Fatalf("retire B, %s at %s: %v", first.b.id, first.address, err)
	}
	var last int
	if err := roll.pool.QueryRow(ctx, `select coalesce(max(number), 0) from artifact_versions where artifact_id = $1::uuid`, first.b.id).Scan(&last); err != nil || last == 0 {
		t.Fatalf("read B's last version: %d (%v)", last, err)
	}
	t.Logf("B's last version is %d", last)
	roll.requireLensLive(t, first, lb, "the retirement", first.address+"@v"+strconv.Itoa(last))

	// ───── the second case: main removes A, cybermonklabs/legion#4173's build makes B ─────
	roll.startEarlierBuilds(t)
	second := roll.lensedDocument(t, "the second case", "Made again by the store's first build.md")
	hold = roll.holdTheLease(t)
	removeTypedDocument(t, roll.main.process, roll.main.name, second.a)
	roll.makeB(t, second, roll.base)
	roll.requireDocumentNode(t, second, graphstore.StateLive, "A's node stands live behind B's row")
	roll.requireStatedByBsCreate(t, second, true, "cybermonklabs/legion#4173's create writes the document's node for B, stating it gone and then live")
	roll.requireAnsweredAsGone(t, second, nil)
	roll.stopEarlierBuilds(t)
	hold.giveBackIndex()
	event = roll.awaitThisBuildsTick(t, second)
	roll.requireGoneAfterTheTick(t, second, event)
	hold.giveBack()

	// ───── the third case: cybermonklabs/legion#4173's build removes A, main makes B ─────
	roll.startEarlierBuilds(t)
	third := roll.lensedDocument(t, "the third case", "Made again over a gone node.md")
	hold = roll.holdTheLease(t)
	removeTypedDocument(t, roll.base.process, roll.base.name, third.a)
	roll.requireDocumentNode(t, third, graphstore.StateGone, "cybermonklabs/legion#4173's removal states A's node gone")
	roll.makeB(t, third, roll.main)
	roll.requireDocumentNode(t, third, graphstore.StateGone, "main's create writes no node, so B's row stands behind A's gone node")
	if before := roll.sendLens(t, dispatchTestLogin, map[string]any{"on": third.address, "kind": staleLensKind}); before.status != http.StatusConflict || before.code() != "NODE_GONE" {
		t.Error(third.lensWriteFailure(before, "before", "409 NODE_GONE"))
		t.Logf("the lens write before the object pass answers %s", before)
	}
	roll.requireAnsweredAsGone(t, third, nil)
	roll.stopEarlierBuilds(t)
	hold.giveBackIndex()
	event = roll.awaitThisBuildsTick(t, third)
	if state := roll.nodeState(t, graphstore.KindArtifact, third.b.ref); state != graphstore.StateLive {
		t.Errorf("the node of %s reads %s after this build's tick ran on %s, want live: the object pass writes it for the row %s",
			third.address, state, event, third.b.id)
	}
	roll.requireGoneAfterTheTick(t, third, event)
	if after := roll.sendLens(t, dispatchTestLogin, map[string]any{"on": third.address, "kind": staleLensKind}); after.status != http.StatusCreated {
		t.Error(third.lensWriteFailure(after, "after", "201"))
		t.Logf("the lens write after the object pass answers %s", after)
	}
	hold.giveBack()
}

// earlierBuild is one build of an earlier release the run serves: what a failure calls it, which
// names the commit it was built from, its binary, and its process while it serves.
type earlierBuild struct {
	name, binary string
	process      *dispatchProcess
}

// staleLensRoll is the run's two earlier builds and this build over one database: the database,
// the test's own connections to it, this build's binary and process, and the spec's issue.
type staleLensRoll struct {
	databaseURL string
	pool        *pgxpool.Pool
	main, base  *earlierBuild
	head        string
	current     *dispatchProcess
	issue       string
}

// newStaleLensRoll reads the two commits and refuses either where its migrations do not match the
// build it names (requireTheStoreMigration), makes the run's empty database (censusDatabase),
// builds main's build, cybermonklabs/legion#4173's and this one, and boots them on that database
// in the order of their releases: main's, then cybermonklabs/legion#4173's, then this build, which
// migrates it to its own schema. Every build serves from then on. This build then makes the
// project and the spec's issue.
func newStaleLensRoll(t *testing.T) *staleLensRoll {
	t.Helper()
	mainCommit := testbaseline.MainCommit(t)
	baseCommit := testbaseline.Commit(t)
	requireTheStoreMigration(t, testbaseline.MainVariable, mainCommit, false)
	requireTheStoreMigration(t, testbaseline.Variable, baseCommit, true)
	databaseURL := censusDatabase(t)
	pool, err := testpool.NewPool(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("open the run's own connections: %v", err)
	}
	t.Cleanup(pool.Close)
	roll := &staleLensRoll{
		databaseURL: databaseURL,
		pool:        pool,
		main: &earlierBuild{name: "main's build (" + mainCommit + ")",
			binary: baseBuildOf(t, mainCommit, testbaseline.MainVariable)},
		base: &earlierBuild{name: "cybermonklabs/legion#4173's build (" + baseCommit + ")",
			binary: baseBuildOf(t, baseCommit, testbaseline.Variable)},
		head: cmdtest.BuildStamped(t, "envoy-dispatch", headBuildStamp),
	}
	roll.startEarlierBuilds(t)
	roll.current = startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: roll.head, SigningKey: staleLensSigningKey})
	roll.current.waitHealthy(t)
	t.Logf("this build, stamped %s, serves beside %s and %s", headBuildStamp, roll.main.name, roll.base.name)
	roll.current.createProject(t, staleLensProject, "Stale lenses")
	roll.issue, _ = createCensusIssue(t, roll.current, staleLensProject, "Documents earlier builds remove and make again",
		"# Spec\n\nThe issue whose documents two earlier builds remove and make again.\n")
	return roll
}

// requireTheStoreMigration fails the test unless commit, the commit variable names, holds the
// migration that made the store (predicateCostStoreMigration) where holds is true, and lacks it
// where holds is false: BASELINE_SHA must name a build of cybermonklabs/legion#4173, whose create
// and removal write a document's node, and MAIN_BASELINE_SHA a build of main from before the
// store, which writes none, or a case would read neither build's writes.
func requireTheStoreMigration(t *testing.T, variable, commit string, holds bool) {
	t.Helper()
	held := holdsTheStore(commit)
	switch {
	case holds && !held:
		t.Fatalf("%s names %s, whose migrations lack 20261109013400 (graph_store, %s), so it is no build of cybermonklabs/legion#4173, whose create writes the document's node and whose removal states it gone: set %s to the merge base of HEAD and that pull request's branch, as this file's run command sets it, or once that pull request has merged to `git merge-base origin/main HEAD`",
			variable, commit, predicateCostStoreMigration, variable)
	case !holds && held:
		t.Fatalf("%s names %s, whose migrations hold 20261109013400 (graph_store, %s), so it is no build of main that writes no node: set %s to `git merge-base origin/main HEAD` while cybermonklabs/legion#4173 is unmerged, or once it has merged to the first parent of its merge commit on main",
			variable, commit, predicateCostStoreMigration, variable)
	}
	t.Logf("%s names %s, which holds the migration that made the store: %t", variable, commit, held)
}

// startEarlierBuilds starts main's build and then cybermonklabs/legion#4173's, in the order of
// their releases, each once the one before it is healthy, on the run's database.
func (r *staleLensRoll) startEarlierBuilds(t *testing.T) {
	t.Helper()
	for _, build := range []*earlierBuild{r.main, r.base} {
		build.process = startDispatchProcessWith(t, r.databaseURL, dispatchOptions{Binary: build.binary, SigningKey: staleLensSigningKey})
		build.process.waitHealthy(t)
		t.Logf("%s serves", build.name)
	}
}

// stopEarlierBuilds stops each earlier build's process as a deploy does, a SIGTERM and a wait for
// it to exit (stopProcess), before the test waits for this build's tick: a period an earlier
// build's process serves is a period this build's tick waits out.
func (r *staleLensRoll) stopEarlierBuilds(t *testing.T) {
	t.Helper()
	for _, build := range []*earlierBuild{r.main, r.base} {
		stopProcess(t, build.process)
		build.process = nil
		t.Logf("%s stopped", build.name)
	}
}

// holdTheLease takes the block-index lease and the reference forms drain's over the run's database
// (holdBlockIndexLease), so no tick of any build and no pass of this build's forms drain runs until
// the hold it answers gives them back: the block-index lease alone before the wait for this
// build's tick (giveBackIndex), and the drain's once the reads after that tick are done
// (giveBack). A test that fails while it holds either gives it back as it ends, before its
// connections close (holdBlockIndexLease).
func (r *staleLensRoll) holdTheLease(t *testing.T) *blockIndexHold {
	t.Helper()
	return holdBlockIndexLease(t, r.pool, "this build", costLeaseWait, r.current.Output.String)
}

// staleLensNode is LO, FO, LP or LB: its address, the kind and the id graph_nodes keys it by, and
// the uuid of the lens its address names, which a read of an address nobody wrote substitutes.
type staleLensNode struct {
	address, kind, id string
	lens              string
}

// staleLensWritten is what POST /api/v1/issues/{key}/lenses answers of a lens it wrote.
type staleLensWritten struct {
	Address  string `json:"address"`
	ID       string `json:"id"`
	Findings []struct {
		Address string `json:"address"`
		ID      string `json:"id"`
	} `json:"findings"`
}

// staleLensOf is the lens a write answered.
func staleLensOf(written staleLensWritten) staleLensNode {
	return staleLensNode{address: written.Address, kind: graphstore.KindLens, id: written.ID, lens: written.ID}
}

func decodeStaleLens(t *testing.T, what string, answer httpAnswer) staleLensWritten {
	t.Helper()
	var written staleLensWritten
	if err := json.Unmarshal(answer.body, &written); err != nil || written.Address == "" || written.ID == "" {
		t.Fatalf("read %s as its write answered it: %v: %s", what, err, answer.body)
	}
	return written
}

// staleLensCase is one case: A, the document the person made and LO stands on, B, the document an
// earlier build made at A's address, that address, and LO, FO and LP.
type staleLensCase struct {
	name       string
	title      string
	a, b       typedDocument
	address    string
	lo, fo, lp staleLensNode
}

// stood is LO, FO and LP.
func (c *staleLensCase) stood() []staleLensNode { return []staleLensNode{c.lo, c.fo, c.lp} }

// readFailure is the words of a read of this build that answers node otherwise than as a gone node
// while B's row stands at the address.
func (c *staleLensCase) readFailure(node staleLensNode, read, status, login string) string {
	return fmt.Sprintf("the lens %s recorded the row %s, and %s answers %s to %s while the row %s stands at %s, want 404 as for a gone node",
		node.address, c.a.id, read, status, login, c.b.id, c.address)
}

// lensWriteFailure is the words of a lens write on B's address that answers otherwise than want.
func (c *staleLensCase) lensWriteFailure(answer httpAnswer, when, want string) string {
	return fmt.Sprintf("the lens write on %s answers %s %s the object pass, want %s",
		c.address, strings.TrimSpace(fmt.Sprintf("%d %s", answer.status, answer.code())), when, want)
}

// lensedDocument makes A through this build by typing title, and LO on it with one finding FO, as
// the person, and LP on FO, reserved for the second person; LO records A's row.
func (r *staleLensRoll) lensedDocument(t *testing.T, name, title string) *staleLensCase {
	t.Helper()
	a := makeTypedDocument(t, r.current, "this build", r.issue, title)
	c := &staleLensCase{name: name, title: title, a: a, address: "dispatch://" + r.issue + "/artifact/" + a.slug}
	invoke := func(what, login string, body map[string]any) staleLensWritten {
		t.Helper()
		answer := r.sendLens(t, login, body)
		if answer.status != http.StatusCreated {
			t.Fatalf("%s: %s", what, answer)
		}
		return decodeStaleLens(t, what, answer)
	}
	lo := invoke("the person's trade-off analysis LO on A", dispatchTestLogin, map[string]any{
		"on": c.address, "kind": staleLensKind, "findings": []string{"FO, the one finding of LO."},
	})
	if len(lo.Findings) != 1 {
		t.Fatalf("LO, %s, answers %d finding(s), want FO alone", lo.Address, len(lo.Findings))
	}
	c.lo = staleLensOf(lo)
	c.fo = staleLensNode{address: lo.Findings[0].Address, kind: refs.StoredKindOf(graphstore.KindLensBlock), id: lo.Findings[0].ID, lens: lo.ID}
	c.lp = staleLensOf(invoke("the second person's lens LP on FO", staleLensSecond, map[string]any{
		"on": c.fo.address, "kind": staleLensKind, "visibility": graphstore.VisibilityAuthor,
	}))
	if got := r.recordedRow(t, c.lo.id); got != a.id {
		t.Errorf("the lens %s on %s records the row %s, want %s, the row at the address when it was written",
			c.lo.address, c.address, staleLensRowWords(got), a.id)
	}
	t.Logf("%s: A is %s at %s, LO %s, FO %s, LP %s", name, a.id, c.address, c.lo.address, c.fo.address, c.lp.address)
	return c
}

// makeB makes B through the earlier build by typing A's title, which takes A's address; a B at
// another address fails the test, since the case then proves nothing.
func (r *staleLensRoll) makeB(t *testing.T, c *staleLensCase, build *earlierBuild) {
	t.Helper()
	c.b = makeTypedDocument(t, build.process, build.name, r.issue, c.title)
	if c.b.ref != c.a.ref {
		t.Fatalf("%s made B at %s, want A's address %s, so %s proves nothing", build.name, c.b.ref, c.a.ref, c.name)
	}
	t.Logf("%s: %s made B, %s, at %s", c.name, build.name, c.b.id, c.address)
}

// sendLens sends login's POST /api/v1/issues/{key}/lenses of body to this build.
func (r *staleLensRoll) sendLens(t *testing.T, login string, body map[string]any) httpAnswer {
	t.Helper()
	return r.current.send(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/lenses", encodeJSON(t, body), asUser(login))
}

// recordedRow is the row lens records (graphstore.AttributeArtifactID) as the store states it now,
// its newest artifact_id tuple, and "" where it records none.
func (r *staleLensRoll) recordedRow(t *testing.T, lens string) string {
	t.Helper()
	value, held, err := graphstore.CurrentValue(context.Background(), r.pool, graphstore.KindLens, lens, graphstore.AttributeArtifactID)
	if err != nil {
		t.Fatalf("read the artifact_id of the lens %s: %v", lens, err)
	}
	if !held {
		return ""
	}
	return value
}

// staleLensRowWords is a row id as a failure names it, and "none" for no row.
func staleLensRowWords(id string) string {
	if id == "" {
		return "none"
	}
	return id
}

// nodeState is the state graph_nodes holds of the node, "(no node)" where it holds none.
func (r *staleLensRoll) nodeState(t *testing.T, kind, id string) string {
	t.Helper()
	var state string
	if err := r.pool.QueryRow(context.Background(), `
		select coalesce((select state from graph_nodes where kind = $1 and id = $2), '(no node)')
	`, kind, id).Scan(&state); err != nil {
		t.Fatalf("read the node %s %s: %v", kind, id, err)
	}
	return state
}

// requireDocumentNode fails the test unless the node at A's address reads state, the case's premise
// premise names.
func (r *staleLensRoll) requireDocumentNode(t *testing.T, c *staleLensCase, state, premise string) {
	t.Helper()
	if held := r.nodeState(t, graphstore.KindArtifact, c.a.ref); held != state {
		t.Fatalf("%s: the node of %s reads %s, want %s: the case's premise is that %s", c.name, c.address, held, state, premise)
	}
}

// createdEvent is the id of B's artifact.created event, the event the earlier build's create of B
// appended.
func (r *staleLensRoll) createdEvent(t *testing.T, c *staleLensCase) int64 {
	t.Helper()
	var event int64
	if err := r.pool.QueryRow(context.Background(), `
		select coalesce(max(id), 0) from events where type = 'artifact.created' and payload->'artifact'->>'id' = $1
	`, c.b.id).Scan(&event); err != nil || event == 0 {
		t.Fatalf("%s: read B's artifact.created event: %d (%v)", c.name, event, err)
	}
	return event
}

// requireStatedByBsCreate fails the test unless the store holds a state tuple on B's address
// caused by B's artifact.created event (createdEvent) where stated is true, and none where it is
// false: whether the earlier build's create of B wrote B's node, the case's premise premise names.
// A node at the address reads live either way, so its state alone cannot tell.
func (r *staleLensRoll) requireStatedByBsCreate(t *testing.T, c *staleLensCase, stated bool, premise string) {
	t.Helper()
	event := r.createdEvent(t, c)
	var tuples int
	if err := r.pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3 and source_event = $4
	`, graphstore.KindArtifact, c.b.ref, graphstore.AttributeState, event).Scan(&tuples); err != nil {
		t.Fatalf("%s: read the state tuples on %s caused by B's artifact.created event %d: %v", c.name, c.address, event, err)
	}
	if (tuples > 0) != stated {
		t.Fatalf("%s: %s holds %d state tuple(s) caused by B's artifact.created event %d: the case's premise is that %s",
			c.name, c.address, tuples, event, premise)
	}
}

// staleLensesRead is what GET /api/v1/lenses answers of a node: the node itself where it is a lens,
// the lenses on it, and the chains they stand in.
type staleLensesRead struct {
	Lens   *staleLensRead  `json:"lens"`
	Lenses []staleLensRead `json:"lenses"`
	Chains []struct {
		Lenses []staleLensRead `json:"lenses"`
	} `json:"chains"`
}

// staleLensRead is one lens of that read: its address, its on and its findings.
type staleLensRead struct {
	Address  string  `json:"address"`
	On       *string `json:"on"`
	Findings []struct {
		Address string `json:"address"`
	} `json:"findings"`
}

// listed is the address of every lens and every finding the read answers, in its lenses and its
// chains.
func (l staleLensesRead) listed() []string {
	var addresses []string
	add := func(lenses []staleLensRead) {
		for _, lens := range lenses {
			addresses = append(addresses, lens.Address)
			for _, finding := range lens.Findings {
				addresses = append(addresses, finding.Address)
			}
		}
	}
	add(l.Lenses)
	for _, chain := range l.Chains {
		add(chain.Lenses)
	}
	return addresses
}

// requireAnsweredAsGone holds this build's reads of LO, FO and LP, with the leases held, to what they
// answer of gone nodes. GET /api/v1/references?to= and GET /api/v1/lenses?on= of each answer 404
// NOT_FOUND, byte for byte as an address nobody wrote answers once the id is substituted, to the
// person, and for LP to the second person too; GET /api/v1/lenses?on= B's address answers 200,
// its on naming the node at B's address with that node's state (gone in the third case, where B's
// row stands behind A's gone node), lists none of them in its lenses or its chains, and where lb
// is not nil answers it alone in its lenses; and the person page of each author lists none of
// them.
func (r *staleLensRoll) requireAnsweredAsGone(t *testing.T, c *staleLensCase, lb *staleLensNode) {
	t.Helper()
	for _, read := range []struct {
		login string
		node  staleLensNode
	}{{dispatchTestLogin, c.lo}, {dispatchTestLogin, c.fo}, {dispatchTestLogin, c.lp}, {staleLensSecond, c.lp}} {
		for _, route := range []string{"/api/v1/references?to=", "/api/v1/lenses?on="} {
			nobody := uuid.NewString()
			own := r.current.send(t, http.MethodGet, route+url.QueryEscape(read.node.address), "", asUser(read.login))
			nobodys := r.current.send(t, http.MethodGet, route+url.QueryEscape(strings.ReplaceAll(read.node.address, read.node.lens, nobody)), "", asUser(read.login))
			nobodysAsOwn := bytes.ReplaceAll(nobodys.body, []byte(nobody), []byte(read.node.lens))
			if own.status != http.StatusNotFound || nobodys.status != http.StatusNotFound || !bytes.Equal(own.body, nobodysAsOwn) {
				t.Error(c.readFailure(read.node, "GET "+route+read.node.address, own.String(), read.login))
				t.Logf("an address nobody wrote answers %s to %s, %s once the id is substituted", nobodys, read.login, nobodysAsOwn)
			}
		}
	}

	read := "GET /api/v1/lenses?on=" + c.address
	answer := r.current.send(t, http.MethodGet, "/api/v1/lenses?on="+url.QueryEscape(c.address), "", asUser(dispatchTestLogin))
	if answer.status != http.StatusOK {
		t.Errorf("%s answers %s to %s, want 200", read, answer, dispatchTestLogin)
	} else {
		var lenses staleLensesRead
		if err := json.Unmarshal(answer.body, &lenses); err != nil {
			t.Fatalf("decode %s: %v: %s", read, err, answer.body)
		}
		listed := lenses.listed()
		for _, node := range c.stood() {
			if slices.Contains(listed, node.address) {
				t.Error(c.readFailure(node, read, "200 listing it in its lenses or its chains", dispatchTestLogin))
			}
		}
		if lb != nil {
			var addresses []string
			for _, lens := range lenses.Lenses {
				addresses = append(addresses, lens.Address)
			}
			if !slices.Equal(addresses, []string{lb.address}) {
				t.Errorf("%s answers the lenses %q to %s, want LB, %s, alone", read, addresses, dispatchTestLogin, lb.address)
			}
		}
	}

	for _, login := range []string{dispatchTestLogin, staleLensSecond} {
		path := "/api/v1/people/" + login + "/nodes?limit=200"
		page := r.current.send(t, http.MethodGet, path, "", asUser(login))
		if page.status != http.StatusOK {
			t.Errorf("GET %s answers %s to %s, want 200: the person page of an author, read by that author", path, page, login)
			continue
		}
		var rows struct {
			Rows []struct {
				ID string `json:"id"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(page.body, &rows); err != nil {
			t.Fatalf("decode GET %s: %v: %s", path, err, page.body)
		}
		listed := make([]string, 0, len(rows.Rows))
		for _, row := range rows.Rows {
			listed = append(listed, row.ID)
		}
		for _, node := range c.stood() {
			if slices.Contains(listed, node.id) {
				t.Error(c.readFailure(node, "GET "+path, "200 listing it in its rows", login))
			}
		}
	}
}

// awaitThisBuildsTick waits, for at most three block-index intervals and a margin, until a tick of
// this build has run on B's artifact.created event: until this build's row of
// block_index_event_cursors has read past the event and owes no object pass, so the tick that
// read it has run every pass it owed, the stale sweep among them (docs' readEventsAfterCursor and
// writeObjectNodesAnotherBuildWrote). It answers the event as a failure names it.
func (r *staleLensRoll) awaitThisBuildsTick(t *testing.T, c *staleLensCase) string {
	t.Helper()
	ctx := context.Background()
	event := r.createdEvent(t, c)
	words := fmt.Sprintf("B's artifact.created event %d", event)
	within := 3*docs.BlockIndexReconcileInterval + 30*time.Second
	deadline := time.Now().Add(within)
	for {
		var cursor *int64
		var owed bool
		if err := r.pool.QueryRow(ctx, `
			select (select event_id from block_index_event_cursors where build_commit = $1),
			       exists (select 1 from block_index_event_cursors where build_commit = $1 and object_nodes_owed_at is not null)
		`, headBuildStamp).Scan(&cursor, &owed); err != nil {
			t.Fatalf("read this build's block-index event cursor: %v", err)
		}
		if cursor != nil && *cursor >= event && !owed {
			t.Logf("%s: this build's tick ran on %s: its cursor reads %d and it owes no object pass", c.name, words, *cursor)
			return words
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: this build's tick did not run on %s within %s of the earlier builds' stop: its cursor reads %v and it owes the object pass: %t\n%s",
				c.name, words, within, cursor, owed, r.current.Output.String())
		}
		time.Sleep(time.Second)
	}
}

// requireGoneAfterTheTick holds LO, FO and LP to gone in the store once this build's tick has run
// on event.
func (r *staleLensRoll) requireGoneAfterTheTick(t *testing.T, c *staleLensCase, event string) {
	t.Helper()
	for _, node := range c.stood() {
		if state := r.nodeState(t, node.kind, node.id); state != graphstore.StateGone {
			t.Errorf("%s reads %s after this build's tick ran on %s, want gone: its recorded row %s no longer stands at %s",
				node.address, state, event, c.a.id, c.address)
		}
	}
}

// requireLensLive holds LB to live in the store after what after names, and GET /api/v1/lenses?on=
// LB to answer its on naming on.
func (r *staleLensRoll) requireLensLive(t *testing.T, c *staleLensCase, lb staleLensNode, after, on string) {
	t.Helper()
	state := r.nodeState(t, lb.kind, lb.id)
	path := "/api/v1/lenses?on=" + url.QueryEscape(lb.address)
	answer := r.current.send(t, http.MethodGet, path, "", asUser(dispatchTestLogin))
	answered := "nothing, as GET /api/v1/lenses?on=" + lb.address + " answers " + answer.String()
	if answer.status == http.StatusOK {
		var read staleLensesRead
		switch err := json.Unmarshal(answer.body, &read); {
		case err != nil:
			t.Fatalf("decode GET /api/v1/lenses?on=%s: %v: %s", lb.address, err, answer.body)
		case read.Lens == nil:
			answered = "nothing, as GET /api/v1/lenses?on=" + lb.address + " answers no lens"
		case read.Lens.On == nil:
			answered = "null"
		default:
			answered = *read.Lens.On
		}
	}
	if state != graphstore.StateLive || answered != on {
		t.Errorf("the lens %s on %s reads %s on %s after %s, want live on %s", lb.address, c.address, state, answered, after, on)
	}
}
