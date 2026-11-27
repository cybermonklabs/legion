//go:build basebuild

package api

// The check c-node-anchor-roll: a comment on a node's text outlives a roll to this build and a
// rollback to the release before it (R31 of the issue-tree spec).
//
// TestANodeAnchorOutlivesARollAndARollback boots this checkout's build and the build of
// BASELINE_SHA, the release before, over one database of its own, in the order a roll and a
// rollback serve it. It carries the basebuild tag and shares the two servers, the run's database
// and the builds of tree_read_skeleton_bound_test.go, because it builds the release before out of
// the repository (git archive), and no CI job builds the tag. It is run on its own, by name, as
//
//	BASELINE_SHA=<the commit the pull request body's BASELINE_SHA= line names> go test -tags basebuild ./internal/dispatch/api/ -run '^TestANodeAnchorOutlivesARollAndARollback$' -timeout 30m -count=1 -v
//
// from packages/envoy, with DISPATCH_TEST_DATABASE_URL naming a Postgres server it may create a
// database on.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/testbaseline"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The issue c-node-anchor-roll writes: its project, its spec, whose requirement r1 holds the words
// each comment of the release before quotes, and the verdict of the lens on r1, which holds the
// word bound twice.
const (
	rollAnchorProject = "ANCH"
	rollAnchorSpec    = ":::requirement{#r1}\nEvery write is idempotent, and its retry is bounded by its key.\n:::\n"
	rollAnchorVerdict = "The write is bound by its key, and the retry is bound by it too."
)

// rollBackfillEnded are the lines this build logs where its boot's run of the backfill ends with
// no done line: skipped at its bound, or failed (cmd/dispatch's runBootBackfill).
var rollBackfillEnded = []string{`msg="dispatch: boot backfill skipped`, `msg="dispatch: boot backfill failed`}

// waitRollBackfill waits until s, a process of this build, logs that its boot's run of the backfill
// is done (skeletonBackfillDone), and fails where it logs that the run ended otherwise, where it
// exits first, or where it logs neither within skeletonBootWait.
func waitRollBackfill(t *testing.T, s *readCostServer) {
	t.Helper()
	ended := ""
	done := testwait.Holds(skeletonBootWait, skeletonBootPoll, func() bool {
		output := s.process.Output.String()
		if strings.Contains(output, skeletonBackfillDone) {
			return true
		}
		for _, line := range rollBackfillEnded {
			if strings.Contains(output, line) {
				ended = line
				return true
			}
		}
		select {
		case <-s.process.Exited:
			ended = "its exit"
			return true
		default:
			return false
		}
	})
	switch {
	case ended != "":
		t.Fatalf("%s's boot backfill ended with %s, not with %s:\n%s", s.name, ended, skeletonBackfillDone, skeletonTail(s.process.Output.String()))
	case !done:
		t.Fatalf("%s logged no %s within %s:\n%s", s.name, skeletonBackfillDone, skeletonBootWait, skeletonTail(s.process.Output.String()))
	}
	t.Logf("c-node-anchor-roll: %s ran its boot backfill", s.name)
}

// writeRollComment writes the comment body on the issue key through s as the writer, a person, and
// answers its id, failing on anything but 201.
func writeRollComment(t *testing.T, s *readCostServer, key, name string, body map[string]any) string {
	t.Helper()
	answer := s.expect(t, "write "+name+" through "+s.name, http.MethodPost, "/api/v1/issues/"+key+"/comments", readCostWriter, body, http.StatusCreated)
	var written struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(answer.body, &written); err != nil || written.ID == "" {
		t.Fatalf("read %s back: %v: %s", name, err, answer)
	}
	return written.ID
}

// rollComments is how many comments rows the issue key holds.
func rollComments(t *testing.T, conn *pgx.Conn, key string) int {
	t.Helper()
	var count int
	if err := conn.QueryRow(context.Background(), `select count(*) from comments where issue_key = $1`, key).Scan(&count); err != nil {
		t.Fatalf("count the comments of %s: %v", key, err)
	}
	return count
}

// rollAnchorEnds is the far end, `<kind>:<id>`, of each anchored_in and each part_of the comment id
// states now, by relation.
func rollAnchorEnds(t *testing.T, conn *pgx.Conn, name, id string) map[string][]string {
	t.Helper()
	stated, err := graphstore.CurrentOut(context.Background(), conn, graphstore.KindComment, id, []string{graphstore.TupleAnchoredIn, graphstore.TuplePartOf})
	if err != nil {
		t.Fatalf("read the anchored_in and the part_of of %s %s: %v", name, id, err)
	}
	ends := map[string][]string{}
	for _, tuple := range stated {
		ends[tuple.Kind] = append(ends[tuple.Kind], tuple.ToKind+":"+tuple.ToID)
	}
	return ends
}

// requireRollAnchorPair fails t unless the comment id states, now, both its anchored_in and its
// part_of to the node kind and node name, printing what it states of each.
func requireRollAnchorPair(t *testing.T, conn *pgx.Conn, name, id, kind, node string) {
	t.Helper()
	ends, want := rollAnchorEnds(t, conn, name, id), kind+":"+node
	if !slices.Contains(ends[graphstore.TupleAnchoredIn], want) || !slices.Contains(ends[graphstore.TuplePartOf], want) {
		t.Errorf("%s %s states anchored_in %v and part_of %v, want both to %s", name, id, ends[graphstore.TupleAnchoredIn], ends[graphstore.TuplePartOf], want)
		return
	}
	t.Logf("c-node-anchor-roll: %s %s states its anchored_in and its part_of to %s", name, id, want)
}

// rollAnchoredBlock is what the stored anchor of the comment id names: its block's id; the address
// of that block's node as the block index addresses it (graphstore.AnchorBlockAddressSQL), which
// is the node its anchored_in and its part_of name while the anchor is not orphaned; whether it is
// orphaned; and the anchor as stored.
func rollAnchoredBlock(t *testing.T, conn *pgx.Conn, id string) (block, address string, orphaned bool, stored string) {
	t.Helper()
	if err := conn.QueryRow(context.Background(), `
		select coalesce(c.anchor->>'block_id', ''), coalesce(`+graphstore.AnchorBlockAddressSQL("c")+`, ''),
		       coalesce((c.anchor->>'orphaned')::boolean, false), coalesce(c.anchor::text, 'null')
		from comments c where c.id = $1::uuid
	`, id).Scan(&block, &address, &orphaned, &stored); err != nil {
		t.Fatalf("read the anchor of comment %s: %v", id, err)
	}
	return block, address, orphaned, stored
}

// rollBlock is one block of a document as GET /api/v1/artifacts/{id}/blocks answers it: its id and
// the range of the document's markdown it spans.
type rollBlock struct {
	ID   string `json:"id"`
	From int    `json:"from"`
	To   int    `json:"to"`
}

// rollAnchorCensusLine is the line `envoy-dispatch census --store` prints of the kind
// comment_anchor_block (graphcensus's StoreReport.Write): its tuples, its rows' triples, and the
// differences in each direction.
var rollAnchorCensusLine = regexp.MustCompile(`(?m)^census: store kind=comment_anchor_block (?:node|tuple)s=(\S+) rows=(\S+) table=\S* rows_without_a_(?:node|tuple)=(\S+) (?:node|tuple)s_without_a_row=(\S+) `)

// requireRollAnchorCensus runs `envoy-dispatch census --store` of binary, this build, over the
// run's database, and fails t unless it reads comment_anchor_block with rows and no difference in
// either direction. The census exits 1 where another kind refuses, which this check does not read,
// and 2 where it could not be taken, which fails t.
func requireRollAnchorCensus(t *testing.T, binary, databaseURL string) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(binary, "census", "--store")
	cmd.Dir = home
	cmd.Env = []string{
		"DATABASE_URL=" + databaseURL,
		"DISPATCH_NATS_DISABLED=1",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + home,
		"XDG_DATA_HOME=" + home,
	}
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		t.Logf("c-node-anchor-roll: envoy-dispatch census --store exits 1, a refusal of a kind other than this check reads, which is read below")
	case err != nil:
		t.Fatalf("envoy-dispatch census --store could not be taken: %v\n%s", err, output)
	}
	line := rollAnchorCensusLine.FindSubmatch(output)
	switch {
	case line == nil:
		t.Errorf("envoy-dispatch census --store prints no line of comment_anchor_block:\n%s", output)
	case string(line[2]) == "0" || string(line[3]) != "0" || string(line[4]) != "0":
		t.Errorf("envoy-dispatch census --store reads comment_anchor_block with %s rows, %s rows without a tuple and %s tuples without a row, want rows and no difference:\n%s",
			line[2], line[3], line[4], output)
	default:
		t.Logf("c-node-anchor-roll: %s", line[0])
	}
}

// TestANodeAnchorOutlivesARollAndARollback is c-node-anchor-roll (R31 of the issue-tree spec). It
// reads BASELINE_SHA first and fails, never skips, where it is unset or names no 40-character
// commit (skeletonBaseCommit), before it builds or boots anything.
//
// It boots this build over an empty database, where a person writes an issue whose spec holds the
// requirement r1, a lens on r1 whose verdict holds bound twice, and a comment on the lens's text by
// node anchor, {node, quote: bound, occurrence: 1}, which states its anchored_in and its part_of to
// the lens. It then boots the build of BASELINE_SHA over the same database beside this build, as a
// roll does, and through that build reads GET /api/v1/issues/{key}/comments, which answers the
// comment on the lens with anchor null; posts a comment with node, which answers 400
// INVALID_ANCHOR and writes no comments row; and writes a comment anchored in r1 by artifact and
// block_id. It stops this build, so the release before serves alone, as after a rollback, and
// writes one more such comment through it. It boots this build again, beside it, and once its boot
// backfill is done each comment the release before wrote holds its anchored_in and its part_of to
// the block its stored anchor names, which is r1 or a block of r1's text; the comment on the lens
// holds both to the lens; and `envoy-dispatch census --store` reads comment_anchor_block with rows
// and at zero differences.
func TestANodeAnchorOutlivesARollAndARollback(t *testing.T) {
	baseCommit := skeletonBaseCommit(t)
	databaseURL := skeletonDatabase(t)
	headCommit := gitOutput(t, ".", "rev-parse", "HEAD")
	if headCommit == baseCommit {
		t.Fatalf("this head's commit %s is %s=%s, so the two builds cannot be told apart", headCommit, testbaseline.Variable, baseCommit)
	}
	baseBinary := skeletonBaseBuild(t, baseCommit)
	module, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("find the module root: %v", err)
	}
	headBinary := cmdtest.BuildCommand(t, cmdtest.Command{
		Name: "envoy-dispatch-head", Dir: module, Target: "./cmd/dispatch",
		Flags: []string{"-ldflags", "-X main.buildCommit=" + headCommit},
	})
	t.Logf("c-node-anchor-roll: the release before runs on the build of %s, the commit %s names; this build on the build of this "+
		"checkout's working copy, stamped with its HEAD %s", baseCommit, testbaseline.Variable, headCommit)
	client := readCostClient()
	listener := skeletonListener(t)
	conn := skeletonConnect(t, databaseURL)

	// ───── first: this build alone, over an empty database ─────
	head := startSkeletonServer(t, client, "this build ("+headCommit+")", headBinary, headCommit, databaseURL, listener)
	waitRollBackfill(t, head)
	for _, person := range []string{readCostReader, readCostWriter} {
		head.expect(t, "sign in "+person, http.MethodGet, "/api/v1/whoami", person, nil, http.StatusOK)
	}
	head.expect(t, "create the project "+rollAnchorProject, http.MethodPost, "/api/v1/projects", readCostWriter,
		map[string]string{"key": rollAnchorProject, "name": "A comment on a lens through a roll"}, http.StatusCreated)
	key := createReadCostIssue(t, head, rollAnchorProject, "A lens a comment is on", rollAnchorSpec, readCostWriter)
	var issue struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	head.read(t, "read "+key, "/api/v1/issues/"+key, &issue)
	spec := issue.PrimaryArtifactID
	if spec == "" {
		t.Fatalf("the issue %s answers no spec", key)
	}
	invoked := head.expect(t, "invoke a lens on r1 of "+key, http.MethodPost, "/api/v1/issues/"+key+"/lenses", readCostWriter, map[string]any{
		"on": refs.BlockRef("dispatch://"+key+"/spec", "r1"), "kind": "trade-off", "verdict": rollAnchorVerdict,
	}, http.StatusCreated)
	var lens lensInvoked
	if err := json.Unmarshal(invoked.body, &lens); err != nil || lens.ID == "" {
		t.Fatalf("read the lens back: %v: %s", err, invoked)
	}
	const lensName = "the comment on the lens"
	lensComment := writeRollComment(t, head, key, lensName, map[string]any{
		"body": "What bounds it?", "anchor": map[string]any{"node": "lens/" + lens.ID, "quote": "bound", "occurrence": 1},
	})
	requireRollAnchorPair(t, conn, lensName+", as this build wrote it,", lensComment, graphstore.KindLens, lens.ID)

	// ───── second: the release before beside this build, as a roll serves them ─────
	base := startSkeletonServer(t, client, "the release before ("+baseCommit+")", baseBinary, baseCommit, databaseURL, listener)
	var listed []map[string]json.RawMessage
	base.read(t, "list the comments of "+key+" through "+base.name, "/api/v1/issues/"+key+"/comments", &listed)
	answered := false
	for _, comment := range listed {
		var id string
		if err := json.Unmarshal(comment["id"], &id); err != nil || id != lensComment {
			continue
		}
		answered = true
		if string(comment["anchor"]) != "null" {
			t.Errorf("%s answers %s %s with anchor %s, want null", base.name, lensName, lensComment, comment["anchor"])
		}
	}
	if !answered {
		t.Errorf("%s lists no %s %s among the %d comments of %s", base.name, lensName, lensComment, len(listed), key)
	}
	before := rollComments(t, conn, key)
	refused, err := base.send(http.MethodPost, "/api/v1/issues/"+key+"/comments", readCostWriter, map[string]any{
		"body": "Refused?", "anchor": map[string]any{"node": "lens/" + lens.ID, "quote": "bound", "occurrence": 1},
	})
	if err != nil {
		t.Fatalf("post a comment with node through %s: %v", base.name, err)
	}
	var refusal refusalAnswer
	_ = json.Unmarshal(refused.body, &refusal)
	if refused.status != http.StatusBadRequest || refusal.Code != "INVALID_ANCHOR" {
		t.Errorf("%s answers a comment with node %s, want 400 INVALID_ANCHOR", base.name, refused)
	}
	if after := rollComments(t, conn, key); after != before {
		t.Errorf("the comment with node %s refused wrote comments: %s holds %d, want %d", base.name, key, after, before)
	}
	type written struct{ name, id string }
	earlier := []written{{name: "the release before's comment during the roll"}, {name: "the release before's comment after the rollback"}}
	earlier[0].id = writeRollComment(t, base, key, earlier[0].name, map[string]any{
		"body": "Idempotent how?", "anchor": map[string]any{"artifact": spec, "block_id": "r1", "quote": "idempotent"},
	})

	// ───── third: the release before alone, as after a rollback ─────
	stopSkeletonServer(t, head)
	earlier[1].id = writeRollComment(t, base, key, earlier[1].name, map[string]any{
		"body": "Bounded by which key?", "anchor": map[string]any{"artifact": spec, "block_id": "r1", "quote": "bounded by its key"},
	})
	for _, comment := range earlier {
		ends := rollAnchorEnds(t, conn, comment.name, comment.id)
		t.Logf("c-node-anchor-roll: %s writes %s %s stating anchored_in %v and part_of %v", base.name, comment.name, comment.id,
			ends[graphstore.TupleAnchoredIn], ends[graphstore.TuplePartOf])
	}

	// ───── fourth: this build again, beside the release before ─────
	again := startSkeletonServer(t, client, "this build again ("+headCommit+")", headBinary, headCommit, databaseURL, listener)
	waitRollBackfill(t, again)
	var blocks []rollBlock
	again.read(t, "read the blocks of the spec of "+key, "/api/v1/artifacts/"+spec+"/blocks", &blocks)
	ranges := make(map[string]rollBlock, len(blocks))
	for _, block := range blocks {
		ranges[block.ID] = block
	}
	requirement, held := ranges["r1"]
	if !held {
		t.Fatalf("the spec of %s lists no block r1: %+v", key, blocks)
	}
	for _, comment := range earlier {
		block, address, orphaned, stored := rollAnchoredBlock(t, conn, comment.id)
		quoted, inSpec := ranges[block]
		switch {
		case orphaned || address == "":
			t.Errorf("%s %s holds the anchor %s, want one naming r1 or a block of its text, not orphaned", comment.name, comment.id, stored)
		case !inSpec || quoted.From < requirement.From || quoted.To > requirement.To:
			t.Errorf("%s %s holds the anchor %s, whose block %q is not r1 (%d to %d) or a block of its text; the spec's blocks are %+v",
				comment.name, comment.id, stored, block, requirement.From, requirement.To, blocks)
		}
		requireRollAnchorPair(t, conn, comment.name, comment.id, refs.BlockNodeKind, address)
	}
	requireRollAnchorPair(t, conn, lensName, lensComment, graphstore.KindLens, lens.ID)
	requireRollAnchorCensus(t, headBinary, databaseURL)
	stopSkeletonServer(t, again)
	stopSkeletonServer(t, base)
}
