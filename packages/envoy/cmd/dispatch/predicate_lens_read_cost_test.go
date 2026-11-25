//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go.

package main

// Check c-read-cost: the reads this child touches get no slower over four
// shapes, read at this head and at the base build in one run.
//
// The test is TestThePredicateAndTheLensReadsGetNoSlower at the end of this file; everything above
// it is the sizes it runs at and the nine reads it measures over each of the four shapes, which
// predicate_lens_read_cost_seed_test.go writes.
// graph_cost_test.go holds the two arms, the clock and the order statistic every cost measurement
// shares, and read_cost_test.go and write_cost_test.go the corpus writers these shapes are built
// from.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testbaseline"
)

const (
	// predicateCostProject is the project every shape hangs under.
	predicateCostProject = "LCOST"
	// predicateCostHeavyBlocks is the first shape: an issue whose main document holds this many
	// blocks, nesting writeCostNesting levels deep, with 40 pointers for each 1,000 blocks of it.
	predicateCostHeavyBlocks = 8_001
	// predicateCostInbound is the second shape: one block with this many tuples into it, which
	// are the pointers of a document written for the purpose.
	predicateCostInbound = 5_000
	// predicateCostHistoryRows is the third shape: a person read while the store holds this many
	// rows of history that are not theirs, written as predicateCostHistoryDocuments documents by
	// readCostOther. predicateCostPersonComments is how many comments the person wrote, so the
	// reads of the shape answer rows of their own out of that history.
	predicateCostHistoryRows      = 50_000
	predicateCostHistoryDocuments = 7
	predicateCostPersonComments   = 200
	// predicateCostChain is the fourth shape, this head's alone: a spec with a chain of this many
	// triggered lenses, half of them reserved for their author. It is the lens read's own depth
	// (api's lensChainDepth), the longest chain that read lists whole, and no scale changes it.
	predicateCostChain = 50
	// predicateCostRequirements is how many requirements each shape's spec marks, each served by
	// one check and one plan task, so the coverage read of every shape reads the node state of
	// each requirement and of what serves it, which is the part of that read this child changed
	// (api/coverage.go's queueRequirementStates and queueServingBlockCounts). The coverage of a
	// spec that marks none answers before either statement runs, so it would measure nothing this
	// child did.
	predicateCostRequirements = 20
	// predicateCostChildren is how many child issues each shape's issue holds, so the issue tree,
	// the list of an issue's children (GET /api/v1/issues?parent=), answers rows of every shape:
	// the tree of an issue with no child answers an empty list, and would measure nothing.
	predicateCostChildren = 20
	// predicateCostRatio is the bound: a read whose p50 at this head is more than this many times
	// its p50 at the base build, in the same run, fails the test.
	predicateCostRatio = 1.25
	// predicateCostPhrase starts the word each shape's main document carries and no other
	// document of the corpus does, so the search of a shape matches that shape's corpus.
	predicateCostPhrase = "quillon"
	// predicateCostIndexWait bounds the wait for the block-index reconciliation to index a corpus.
	predicateCostIndexWait = 30 * time.Minute
	// predicateCostScaleTestEnv gates the run, as api/inbox_dismissed_scale_test.go's
	// dismissedScaleEnv gates a14: building the base commit, writing about 70,000 blocks into
	// each of two databases and taking about 12,000 timed readings costs tens of minutes, which
	// no ordinary run should pay. No CI job builds the basebuild tag, so CI never runs it.
	predicateCostScaleTestEnv = "LEGION_SCALE_TEST"
	// predicateCostScaleEnv is the share of the check's sizes a run writes and reads: a fraction
	// above 0 and at most 1, and 1 where it is unset, which is the check. Below 1 the run is a
	// smoke of this test and no reading of the check: every shape it states carries the size it
	// was written at, and once it has measured and printed every reading it ends in SKIP, which
	// the check's run string refuses.
	predicateCostScaleEnv = "LEGION_READ_COST_SCALE"
	// predicateCostControlEnv is the check's negative control, which this test reads and no
	// server does. Set to predicateCostDropInbound, the run drops predicateCostInboundIndexes
	// from this head's database once its corpus is written and analysed, and prints the plan of
	// the tuples-into-a-node lookup before and after the drop. Unset, the run drops nothing. A
	// run with it set is no reading of the check: one whose every read holds the bound ends in
	// SKIP, a control that did not bite, which the check's run string refuses.
	predicateCostControlEnv  = "LEGION_READ_COST_CONTROL"
	predicateCostDropInbound = "drop-inbound-indexes"
)

// predicateCostInboundIndexes are the indexes the check's negative control drops:
// graph_tuples_as_of_in alone, which migration 20261109013400_graph_store declares, the one index
// of the store's set that serves a read into a node.
var predicateCostInboundIndexes = []string{"graph_tuples_as_of_in"}

// predicateCostJITOff is the setting both builds' databases carry from before either server starts
// (startCostArmsWith), so every reading is taken with JIT off, as production runs. Production's
// database is Cloud SQL, which does not support PostgreSQL's LLVM JIT, and under Postgres's
// default jit = on a reading times compilation production never pays: over the history shape, JIT
// took about 1.3 s of each of the references read's two 1.3 s statements. benchtime.Open turns JIT
// off for the same reason.
var predicateCostJITOff = map[string]string{"jit": "off"}

// predicateCostSessionJIT is what the session that runs it reads of JIT: the jit it runs under, and
// the settings its database holds for every role (pg_db_role_setting's row with setrole 0), empty
// where it holds none.
const predicateCostSessionJIT = `select current_setting('jit'), coalesce((select s.setconfig from pg_db_role_setting s
	join pg_database d on d.oid = s.setdatabase where d.datname = current_database() and s.setrole = 0), '{}')`

// predicateCostInboundLookup is the lookup the tuples-into-a-node read makes of one block, $1 its
// node id, read as a value before the read as readEdgeRows reads it: the edges of graph_edges into
// it, newest first, as refs' readEdgeRows selects them, less the predicate of each far end, which
// reads graph_nodes and no index of graph_tuples.
const predicateCostInboundLookup = `
	select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq
	from graph_edges g where g.to_kind = 'block' and g.to_id = (select $1::text)
	order by g.created_at desc, g.kind, g.from_kind, g.from_id`

// predicateCostControlOf reads LEGION_READ_COST_CONTROL: empty where it is unset, which is the
// check, and predicateCostDropInbound where it names the check's negative control.
func predicateCostControlOf(t *testing.T) string {
	t.Helper()
	switch control := strings.TrimSpace(os.Getenv(predicateCostControlEnv)); control {
	case "", predicateCostDropInbound:
		return control
	default:
		t.Fatalf("%s=%q: want %q, the check's negative control, or nothing", predicateCostControlEnv, control, predicateCostDropInbound)
		return ""
	}
}

// predicateCostKinds are the lens_kind ids of the catalogue the fourth shape's chain takes in turn.
var predicateCostKinds = []string{"trade-off", "inversion", "botec", "second-order"}

// predicateCostSizes is what one run writes and reads: the check's sizes, or the share of them
// LEGION_READ_COST_SCALE names.
type predicateCostSizes struct {
	scale                                            float64
	heavyBlocks, inbound, historyRows, historyBlocks int
	personComments                                   int
	// readings is how many readings each read takes over each shape on each build:
	// costReadReadings, 200, at the check's sizes.
	readings int
}

// predicateCostSizesOf reads LEGION_READ_COST_SCALE and answers the sizes of this run. Each share
// keeps a floor under it that keeps the shape's own structure: the eight levels with a pointer in
// each document, more than one pointer at the inbound node, and twenty readings to a cell.
func predicateCostSizesOf(t *testing.T) predicateCostSizes {
	t.Helper()
	scale := 1.0
	if raw := strings.TrimSpace(os.Getenv(predicateCostScaleEnv)); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || parsed <= 0 || parsed > 1 {
			t.Fatalf("%s=%q: want a fraction above 0 and at most 1, the share of the check's sizes this run writes",
				predicateCostScaleEnv, raw)
		}
		scale = parsed
	}
	share := func(full, floor int) int { return max(int(math.Round(float64(full)*scale)), floor) }
	historyRows := share(predicateCostHistoryRows, predicateCostHistoryDocuments*100)
	return predicateCostSizes{
		scale:          scale,
		heavyBlocks:    share(predicateCostHeavyBlocks, 100),
		inbound:        share(predicateCostInbound, 10),
		historyRows:    historyRows,
		historyBlocks:  (historyRows + predicateCostHistoryDocuments - 1) / predicateCostHistoryDocuments,
		personComments: share(predicateCostPersonComments, 10),
		readings:       share(costReadReadings, 20),
	}
}

// The four shapes as the check states them, at the sizes of this run.
func (s predicateCostSizes) heavyName() string {
	return "an issue of " + predicateCostCount(s.heavyBlocks) + " blocks nested eight deep"
}

func (s predicateCostSizes) inboundName() string {
	return "a node with " + predicateCostCount(s.inbound) + " inbound tuples"
}

func (s predicateCostSizes) historyName() string {
	return "a person with " + predicateCostCount(s.historyRows) + " rows of unrelated history"
}

func (predicateCostSizes) lensName() string {
	return "a spec with a chain of " + strconv.Itoa(predicateCostChain) +
		" triggered lenses, half of them reserved for their author"
}

// predicateCostLensRead is what the fourth shape's guard reads of GET /api/v1/lenses: each lens's
// address and visibility, and each chain with the lens its walk stopped at.
type predicateCostLensRead struct {
	Lenses []predicateCostLens `json:"lenses"`
	Chains []struct {
		Lenses    []predicateCostLens `json:"lenses"`
		StoppedAt *string             `json:"stopped_at"`
	} `json:"chains"`
}

type predicateCostLens struct {
	Address    string `json:"address"`
	Visibility string `json:"visibility"`
}

// predicateCostLensesHold holds the fourth shape to its statement, through the lens read its
// author takes: the 50 lenses on the spec in the order they were invoked, one chain of all 50
// that the walk lists whole with no stop, and 25 of them reserved for their author. A chain that
// did not form would otherwise be measured as a fast one.
func predicateCostLensesHold(t *testing.T, arm *costArm, shape *predicateCostShape) []string {
	t.Helper()
	read := arm.server.call(t, http.MethodGet, "/api/v1/lenses?on="+url.QueryEscape(shape.documentRef), "", http.StatusOK)
	var answer predicateCostLensRead
	if err := json.Unmarshal(read, &answer); err != nil {
		t.Fatalf("%s: read the lenses on %s: %v: %s", arm.name, shape.documentRef, err, read)
	}
	addresses := func(lenses []predicateCostLens) []string {
		listed := make([]string, len(lenses))
		for index, lens := range lenses {
			listed[index] = lens.Address
		}
		return listed
	}
	if got := strings.Join(addresses(answer.Lenses), " "); got != strings.Join(shape.lenses, " ") {
		t.Fatalf("%s: the lens read of %s lists %d lens(es), and the shape invoked %d on it in this order: %v",
			arm.name, shape.documentRef, len(answer.Lenses), len(shape.lenses), shape.lenses)
	}
	if len(answer.Chains) != 1 || answer.Chains[0].StoppedAt != nil ||
		strings.Join(addresses(answer.Chains[0].Lenses), " ") != strings.Join(shape.lenses, " ") {
		t.Fatalf("%s: the lens read of %s answers %d chain(s), and the shape is one chain of all %d lenses listed whole with no stop: %s",
			arm.name, shape.documentRef, len(answer.Chains), predicateCostChain, read)
	}
	authorOnly := 0
	for _, lens := range answer.Lenses {
		if lens.Visibility == "author" {
			authorOnly++
		}
	}
	if authorOnly != predicateCostChain/2 {
		t.Fatalf("%s: %d of the %d lenses on %s are reserved for their author, and the shape states half of them",
			arm.name, authorOnly, predicateCostChain, shape.documentRef)
	}
	triggers := arm.count(t, `
		select count(*) from graph_tuples g join graph_nodes n on n.kind = g.from_kind and n.id = g.from_id
		where g.kind = 'triggered_by' and g.asserted and n.kind = 'lens'`)
	return []string{
		fmt.Sprintf("%d lenses on the spec, which the lens read lists as one chain of %d with no stop",
			len(answer.Lenses), len(answer.Chains[0].Lenses)),
		fmt.Sprintf("%d of them reserved for their author", authorOnly),
		fmt.Sprintf("%d triggered_by tuple(s) between them", triggers),
	}
}

// ───── what each shape holds ──────────────────────────────────────────────

// settlePredicateCost finishes each shape once its build has indexed it: it resolves the node the
// tuples-out read starts from, which is the server's own answer and not a guess, holds the shape
// to its statement, to the coverage and to the children it was written with, and prints what it
// holds.
func settlePredicateCost(t *testing.T, arm *costArm, shapes []*predicateCostShape) {
	t.Helper()
	for _, shape := range shapes {
		if shape.outOf == "" {
			shape.outOf = readCostPointerAt(t, arm, shape.into, readCostEdges(t, arm, "to", shape.into))
		}
		shape.rows = append(shape.holds(t, arm, shape),
			fmt.Sprintf("%d block(s) of the issue's documents", arm.count(t,
				`select count(*) from document_blocks b join artifacts a on a.id = b.artifact_id where a.issue_key = $1`, shape.issue)),
			fmt.Sprintf("%d block reference(s)", arm.count(t,
				`select count(*) from block_refs r join artifacts a on a.id = r.artifact_id where a.issue_key = $1`, shape.issue)),
			fmt.Sprintf("%d comment(s)", arm.count(t, `select count(*) from comments where issue_key = $1`, shape.issue)),
			predicateCostCoverageHolds(t, arm, shape),
			predicateCostTreeHolds(t, arm, shape))
		t.Logf("%s: the shape %q holds %s", arm.name, shape.name, strings.Join(shape.rows, ", "))
	}
}

// predicateCostCoverageHolds holds the shape's issue to the coverage it was written with - every
// requirement of its spec served by a check and a plan task - so the coverage read measured is one
// that reads what serves each requirement, and answers that as a row of the shape.
func predicateCostCoverageHolds(t *testing.T, arm *costArm, shape *predicateCostShape) string {
	t.Helper()
	read := arm.server.call(t, http.MethodGet, "/api/v1/issues/"+shape.issue+"/coverage", "", http.StatusOK)
	var coverage struct {
		Requirements []struct {
			BlockID   string `json:"block_id"`
			Checks    int    `json:"checks"`
			PlanTasks int    `json:"plan_tasks"`
		} `json:"requirements"`
	}
	if err := json.Unmarshal(read, &coverage); err != nil {
		t.Fatalf("%s: read the coverage of %s: %v: %s", arm.name, shape.issue, err, read)
	}
	served := 0
	for _, requirement := range coverage.Requirements {
		if requirement.Checks > 0 && requirement.PlanTasks > 0 {
			served++
		}
	}
	if len(coverage.Requirements) != predicateCostRequirements || served != predicateCostRequirements {
		t.Fatalf("%s: the coverage of %s answers %d requirement(s), %d of them served by a check and a plan task, and the shape's spec marks %d, each served: %s",
			arm.name, shape.issue, len(coverage.Requirements), served, predicateCostRequirements, read)
	}
	return fmt.Sprintf("%d requirement(s) of its spec, each served by a check and a plan task", served)
}

// predicateCostTreePath is the issue tree of the shape's issue, the list of its children: GET
// /api/v1/issues with parent, which listIssues filters on the parent's key.
func predicateCostTreePath(shape *predicateCostShape) string {
	return "/api/v1/issues?parent=" + url.QueryEscape(shape.issue)
}

// predicateCostTreeHolds holds the shape's issue to the children it was written with, through the
// issue tree the reads measure: the predicateCostChildren issues the seed wrote under it, each
// naming it as its parent, so the tree measured answers rows of the shape.
func predicateCostTreeHolds(t *testing.T, arm *costArm, shape *predicateCostShape) string {
	t.Helper()
	read := arm.server.call(t, http.MethodGet, predicateCostTreePath(shape), "", http.StatusOK)
	var children []struct {
		Key    string  `json:"key"`
		Parent *string `json:"parent"`
	}
	if err := json.Unmarshal(read, &children); err != nil {
		t.Fatalf("%s: read the issue tree of %s: %v: %s", arm.name, shape.issue, err, read)
	}
	for _, child := range children {
		if child.Parent == nil || *child.Parent != shape.issue {
			t.Fatalf("%s: the issue tree of %s lists %s, whose parent is not %s: %s", arm.name, shape.issue, child.Key, shape.issue, read)
		}
	}
	if len(children) != predicateCostChildren {
		t.Fatalf("%s: the issue tree of %s lists %d child issue(s), and the shape wrote %d under it: %s",
			arm.name, shape.issue, len(children), predicateCostChildren, read)
	}
	return fmt.Sprintf("%d child issue(s), which the issue tree lists", len(children))
}

// requirePredicateCostAuthorNodes holds the first three shapes to holding no node reserved for its
// author: no block of a document takes `author`, so every node of the store reserved for its
// author is a lens of the fourth shape or a finding of one (a block whose id names its lens).
func requirePredicateCostAuthorNodes(t *testing.T, head *costArm) {
	t.Helper()
	elsewhere := head.count(t, `
		select count(*) from graph_nodes
		where visibility = 'author' and kind <> 'lens' and not (kind = 'block' and id like 'lens/%')`)
	lensNodes := head.count(t, `
		select count(*) from graph_nodes
		where visibility = 'author' and (kind = 'lens' or (kind = 'block' and id like 'lens/%'))`)
	t.Logf("%s: the first three shapes hold %d node(s) reserved for its author, because no block of a document takes author; "+
		"the fourth holds %d, its author-only lenses and their findings", head.name, elsewhere, lensNodes)
	if elsewhere != 0 {
		t.Fatalf("%s: the store holds %d node(s) reserved for its author outside the fourth shape's lenses and findings, and the first three shapes state none",
			head.name, elsewhere)
	}
}

// dropPredicateCostInboundIndexes is the check's negative control on this head's database: it
// drops predicateCostInboundIndexes, and prints the plan of the lookup the tuples-into-a-node read
// makes of the node of shape before the drop and after it, so the run shows the read losing the
// index rather than assuming it. A plan that read none of them before the drop would make the
// drop no control of this read, and fails the run.
func dropPredicateCostInboundIndexes(t *testing.T, head *costArm, shape *predicateCostShape) {
	t.Helper()
	var node string
	if err := head.pool.QueryRow(context.Background(), `
		select b.node_id from document_blocks b join artifacts a on a.id = b.artifact_id
		where a.issue_key = $1 and b.block_id = $2`, shape.issue, predicateCostBlockID(shape.into)).Scan(&node); err != nil {
		t.Fatalf("%s: read the node id of %s, the node of %q: %v", head.name, shape.into, shape.name, err)
	}
	reads := func(plan string) []string {
		return slices.DeleteFunc(slices.Clone(predicateCostInboundIndexes), func(index string) bool { return !strings.Contains(plan, index) })
	}
	before := predicateCostInboundPlan(t, head, node)
	t.Logf("%s=%s: %s's plan of the tuples into %s, the node of %q, before the drop (it reads %v):\n%s",
		predicateCostControlEnv, predicateCostDropInbound, head.name, shape.into, shape.name, reads(before), before)
	if len(reads(before)) == 0 {
		t.Fatalf("%s=%s: %s's plan of the tuples into %s reads none of %v before the drop, so the drop is no control of that read",
			predicateCostControlEnv, predicateCostDropInbound, head.name, shape.into, predicateCostInboundIndexes)
	}
	for _, index := range predicateCostInboundIndexes {
		if _, err := head.pool.Exec(context.Background(), "drop index "+index); err != nil {
			t.Fatalf("%s=%s: drop %s from %s's database: %v", predicateCostControlEnv, predicateCostDropInbound, index, head.name, err)
		}
	}
	after := predicateCostInboundPlan(t, head, node)
	t.Logf("%s=%s: %s's plan of the tuples into %s after the drop of %v from its database:\n%s",
		predicateCostControlEnv, predicateCostDropInbound, head.name, shape.into, predicateCostInboundIndexes, after)
	if left := reads(after); len(left) != 0 {
		t.Fatalf("%s=%s: %s's plan of the tuples into %s still reads %v after the drop", predicateCostControlEnv, predicateCostDropInbound,
			head.name, shape.into, left)
	}
}

// predicateCostInboundPlan is the plan Postgres makes on the arm's database of
// predicateCostInboundLookup for node, the node id of one block, as EXPLAIN prints it.
func predicateCostInboundPlan(t *testing.T, arm *costArm, node string) string {
	t.Helper()
	rows, err := arm.pool.Query(context.Background(), "explain "+predicateCostInboundLookup, node)
	if err != nil {
		t.Fatalf("%s: explain the tuples into %s: %v", arm.name, node, err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: explain the tuples into %s: %v", arm.name, node, err)
	}
	return strings.Join(lines, "\n")
}

// ───── the nine reads ─────────────────────────────────────────────────────

// predicateCostRead is one read this child touches, measured over each of the four shapes. Its
// name says which of the check's reads it is and the route it is read through.
type predicateCostRead struct {
	name string
	// headOnly marks the read the base build does not serve at all, which this child adds: it
	// has no reading at the base build, so it is printed and bound nothing.
	headOnly bool
	// cutAt is the code of the one refusal either build may answer this read with, a read cut at
	// the closure bound main's #4554 puts on it (predicateCostClosureBound). Such a reading counts
	// as predicateCostClosureBound at either build, so a read that costs the same at both builds
	// reads the same at both, and the run prints how many readings each build cut. Any other
	// refusal at either build fails.
	cutAt string
	path  func(shape *predicateCostShape) string
}

// predicateCostClosureBound is the bound main's #4554 puts on an issue's reference closure walk
// (api's referencesClosureBound): a GET of an issue's references whose walk runs past it answers
// 503 REFERENCES_TIMEOUT. Over the shape of a person with 50,000 rows of unrelated history the
// read's p50 was 0.9 to 1.4 s over seven runs on the build farm, the two builds within 4% of each
// other in each run: close enough to the bound that either build may cut a reading of it there.
const predicateCostClosureBound = 2 * time.Second

func predicateCostReads() []predicateCostRead {
	return []predicateCostRead{
		{name: "the list of a kind (GET /api/v1/block-links)", path: func(shape *predicateCostShape) string {
			return "/api/v1/block-links?from=" + url.QueryEscape(shape.documentRef) + "&to=" + url.QueryEscape(shape.linksTo)
		}},
		{name: "search (GET /api/v1/search)", path: func(shape *predicateCostShape) string {
			return "/api/v1/search?project=" + predicateCostProject + "&q=" + url.QueryEscape(shape.phrase)
		}},
		{name: "the tuples out of a node (GET /api/v1/references?from=)", path: func(shape *predicateCostShape) string {
			return "/api/v1/references?from=" + url.QueryEscape(shape.outOf)
		}},
		{name: "the tuples into a node (GET /api/v1/references?to=)", path: func(shape *predicateCostShape) string {
			return "/api/v1/references?to=" + url.QueryEscape(shape.into)
		}},
		{name: "the backlink counts (GET /api/v1/issues/{key})", path: func(shape *predicateCostShape) string {
			return "/api/v1/issues/" + shape.issue
		}},
		{name: "the issue tree (GET /api/v1/issues?parent=)", path: predicateCostTreePath},
		{name: "GET /api/v1/lenses with its chains", headOnly: true, path: func(shape *predicateCostShape) string {
			return "/api/v1/lenses?on=" + url.QueryEscape(shape.documentRef)
		}},
		{name: "the coverage read (GET /api/v1/issues/{key}/coverage)", path: func(shape *predicateCostShape) string {
			return "/api/v1/issues/" + shape.issue + "/coverage"
		}},
		{name: "the references panel read (GET /api/v1/issues/{key}/references)", cutAt: "REFERENCES_TIMEOUT", path: func(shape *predicateCostShape) string {
			return "/api/v1/issues/" + shape.issue + "/references"
		}},
	}
}

// predicateCostCount is a count as the check writes one, with a comma between each three digits.
func predicateCostCount(count int) string {
	digits := strconv.Itoa(count)
	var written strings.Builder
	for index, digit := range digits {
		if index > 0 && (len(digits)-index)%3 == 0 {
			written.WriteByte(',')
		}
		written.WriteRune(digit)
	}
	return written.String()
}

// predicateCostDocumentOf is the address of the document a block address `<document>#b-<id>`
// names a block of.
func predicateCostDocumentOf(ref string) string {
	if cut := strings.LastIndex(ref, "#b-"); cut >= 0 {
		return ref[:cut]
	}
	return ref
}

// predicateCostBlockID is the block id a block address `<document>#b-<id>` names.
func predicateCostBlockID(ref string) string {
	if cut := strings.LastIndex(ref, "#b-"); cut >= 0 {
		return ref[cut+len("#b-"):]
	}
	return ref
}

// predicateCostShapePhrase is the word one shape's main document carries and no other document of
// the corpus does: a search that matched every shape would be one read measured four times.
func predicateCostShapePhrase(shape string) string { return predicateCostPhrase + shape }

// ───── the run ────────────────────────────────────────────────────────────

// The reads this child touches get no slower, over the shapes that can make one ruinous.
//
// This child adds five kinds to the reads of the graph and changes what each read takes as
// present (a node not gone, rather than a node live), so a read that is unchanged on one shape can
// be ruinous on another. The run states its four shapes and prints the row counts of each: an
// issue of 8,001 blocks nested eight deep, a node with 5,000 inbound tuples and a person with
// 50,000 rows of unrelated history, which both builds write through their own APIs with the same
// requests; and, on this head alone, a spec with a chain of 50 triggered lenses, half of them
// reserved for their author. The first three shapes hold no node reserved for its author, because
// no block of a document takes `author`, and the run asserts that of this head's store.
//
// It measures nine reads over each shape: the list of a kind, search, the tuples out of a node, the
// tuples into a node, the backlink counts, the issue tree (the list of the shape's issue's
// children, each shape's issue holding predicateCostChildren), GET /api/v1/lenses with its chains,
// the coverage read and the references panel read, each named with its route. The base build is the
// commit BASELINE_SHA names, which c-read-cost's run string sets, where it is unset, to the merge
// base with the branch of cybermonklabs/legion#4173, the release this child lands after; a base
// whose tree holds no migration 20261109013400 (graph_store) is refused before anything is
// built, naming the base (predicateCostBaseHoldsTheStore), since a base with no store measures
// nothing this child changed.
// Each build serves a database of its own (startCostArmsWith), which holds jit = off from before
// that build's server starts, as production's Cloud SQL runs with JIT off (predicateCostJITOff);
// the run reads the setting on each before it seeds. Both builds are read in one loop, one reading
// on each in each turn, the build that goes first alternating, after one warm-up reading on each
// that is not counted (costInterleaved). The corpus is vacuumed and analysed after the seed and the
// backfill (costArm.vacuumAnalyze), and the block-index lease of each build is held while the
// readings are taken (costHoldBlockIndex).
// It takes 200 readings of each read over each shape on each build and prints the nearest-rank
// p50, p95 and p99. A read whose p50 at this head is more than 1.25 times its p50 at the base
// build fails the test, and the failure prints the read, the shape and both builds' readings. The
// p95 and the p99 bind nothing. GET /api/v1/lenses and the fourth shape have no reading at the
// base build, so they are printed and bind nothing either.
//
// It runs only where LEGION_SCALE_TEST=1 is set, under the basebuild tag, which no CI job builds.
// LEGION_READ_COST_SCALE writes and reads a share of these sizes, for a smoke of the test that is
// no reading of the check: such a run measures and prints every reading and then ends in SKIP.
// LEGION_READ_COST_CONTROL set to drop-inbound-indexes runs the check's negative control
// (predicateCostControlEnv), which fails the run, or ends it in SKIP where no read crossed the
// bound, a control that did not bite.
func TestThePredicateAndTheLensReadsGetNoSlower(t *testing.T) {
	if os.Getenv(predicateCostScaleTestEnv) != "1" {
		t.Skipf("set %s=1 to run c-read-cost: it builds the base commit, writes about 70,000 blocks into each of two databases and takes about 12,000 timed readings",
			predicateCostScaleTestEnv)
	}
	sizes := predicateCostSizesOf(t)
	control := predicateCostControlOf(t)
	predicateCostBaseHoldsTheStore(t)
	t.Log(costGVisorNote)
	t.Log("how each build is driven: both are server processes of their own build, each serving a " +
		"database of its own which that build migrated at boot, and each is driven over its own HTTP " +
		"API by this test, which encodes every request before the clock starts and reads every answer " +
		"to its end before it stops. This head is read after its backfill, so every reading of it is a " +
		"reading of the store's own arm.")
	if sizes.scale != 1 {
		t.Logf("%s=%g: this run writes and reads that share of the check's sizes, so it is a smoke of this test and "+
			"no reading of c-read-cost; every shape below states the size it was written at, and the run ends in SKIP "+
			"once every reading is printed", predicateCostScaleEnv, sizes.scale)
	}
	if control != "" {
		t.Logf("%s=%s: this run is the check's negative control, which drops %v from this head's database once its corpus "+
			"is written and analysed, and is no reading of c-read-cost", predicateCostControlEnv, control, predicateCostInboundIndexes)
	}
	t.Logf("the shapes: %q, %q and %q, which both builds write; and %q, which this head alone writes. "+
		"Each read takes %d reading(s) over each shape on each build that serves it, after %d warm-up reading(s) not counted",
		sizes.heavyName(), sizes.inboundName(), sizes.historyName(), sizes.lensName(), sizes.readings, costWarmUp)

	listener := costEnvoy(t)
	listener.Accept(true)
	head, base := startCostArmsWith(t, listener.URL, predicateCostJITOff)
	builds := []*costArm{head, base}
	for _, arm := range builds {
		var jit string
		var held []string
		if err := arm.pool.QueryRow(context.Background(), predicateCostSessionJIT).Scan(&jit, &held); err != nil {
			t.Fatalf("%s: read jit and its database's settings: %v", arm.name, err)
		}
		if jit != "off" || !slices.Contains(held, "jit=off") {
			t.Fatalf("%s: jit reads %q on this test's own session and the build's database holds %q, want off and a setting jit=off",
				arm.name, jit, held)
		}
	}
	t.Log("JIT is off on both builds, as production runs: each build's database holds jit = off, set before its server " +
		"started, so every session either server opens starts with JIT off, and the test's own session on each reads it. " +
		"Production's Cloud SQL does not support PostgreSQL's LLVM JIT, so a reading under jit = on would time compilation " +
		"production never pays")
	shapes := map[*costArm][]*predicateCostShape{}
	for _, arm := range builds {
		shapes[arm] = seedPredicateCost(t, arm, sizes)
	}
	shapes[head] = append(shapes[head], seedPredicateCostLenses(t, head, sizes))
	costRunBackfill(t, head)
	for _, arm := range builds {
		costBlockIndexDrained(t, arm, predicateCostIndexWait)
		arm.vacuumAnalyze(t)
		t.Logf("%s: the whole corpus holds %d document block(s), %d block reference(s) and %d comment(s)%s",
			arm.name,
			arm.count(t, `select count(*) from document_blocks`),
			arm.count(t, `select count(*) from block_refs`),
			arm.count(t, `select count(*) from comments`),
			costStoreCounts(t, arm))
		settlePredicateCost(t, arm, shapes[arm])
	}
	requirePredicateCostAuthorNodes(t, head)
	if control == predicateCostDropInbound {
		inbound := slices.IndexFunc(shapes[head], func(shape *predicateCostShape) bool { return shape.name == sizes.inboundName() })
		if inbound < 0 {
			t.Fatalf("%s holds no shape %q for the control to read", head.name, sizes.inboundName())
		}
		dropPredicateCostInboundIndexes(t, head, shapes[head][inbound])
	}

	// No tick of either server's own runs inside a measured window.
	for _, arm := range builds {
		defer costHoldBlockIndex(t, arm)()
	}

	reads := predicateCostReads()
	readings := []costReading{}
	held := map[string]costReading{}
	cell := func(read string, shape int, arm *costArm) string {
		return read + "\x00" + strconv.Itoa(shape) + "\x00" + arm.name
	}
	for _, read := range reads {
		for index, shape := range shapes[head] {
			measured := builds
			if read.headOnly || shape.headOnly {
				measured = []*costArm{head}
			}
			cut := map[*costArm]int{}
			taken := costInterleaved(t, read.name, shape.name, sizes.readings, measured, func(subject *costArm, run int) time.Duration {
				subject.ready(t)
				path := read.path(shapes[subject][index])
				if read.cutAt == "" {
					reading, _ := subject.timed(t, http.MethodGet, path, "", http.StatusOK)
					return reading
				}
				reading, answer := subject.timedAnswer(t, http.MethodGet, path, "")
				if answer.status == http.StatusServiceUnavailable && answer.code() == read.cutAt {
					if run >= 0 {
						cut[subject]++
					}
					return predicateCostClosureBound
				}
				if answer.status != http.StatusOK {
					t.Fatalf("%s: GET %s: status %d, want 200 or 503 %s: %s", subject.name, path, answer.status, read.cutAt, answer.body)
				}
				return reading
			})
			for _, subject := range measured {
				if read.cutAt != "" {
					t.Logf("%s over %q: %s cut %d of its %d readings at its %s bound (%s), each counted as %s; its p50 with them %.1f ms",
						read.name, shape.name, subject.name, cut[subject], sizes.readings, predicateCostClosureBound, read.cutAt,
						predicateCostClosureBound, costMilliseconds(taken[subject].p50()))
				}
				readings = append(readings, taken[subject])
				held[cell(read.name, index, subject)] = taken[subject]
			}
		}
	}
	costTableOf(t, readings)

	t.Logf("the bound: a read whose p50 at %s is more than %.2f times its p50 at %s fails; the p95 and the p99 are printed and bind nothing",
		head.name, predicateCostRatio, base.name)
	// A smoke reads too few readings over too small a corpus to be a reading of the bound, so it
	// prints a cell over the bound in the same words and fails nothing, and ends in the SKIP below:
	// a test Go reports failed and then skipped is reported FAIL.
	overBound, crossed := t.Errorf, false
	if sizes.scale != 1 {
		overBound = t.Logf
	}
	for _, read := range reads {
		for index, shape := range shapes[head] {
			here := held[cell(read.name, index, head)]
			if read.headOnly || shape.headOnly {
				why := "the base build holds no such shape, a lens being this child's kind"
				if read.headOnly {
					why = "the base build serves no such read, which this child adds"
				}
				t.Logf("%s over %q: p50 %.1f ms, p95 %.1f ms, p99 %.1f ms at %s. It has no reading at the base build, because %s, so it is printed and binds nothing.",
					read.name, shape.name, costMilliseconds(here.p50()), costMilliseconds(here.percentile(0.95)),
					costMilliseconds(here.p99()), head.name, why)
				continue
			}
			before := held[cell(read.name, index, base)]
			ratio := float64(here.p50()) / float64(before.p50())
			t.Logf("%s over %q: p50 %.1f ms at %s against %.1f ms at %s, %.2f times it, against the %.2f bound "+
				"(p95 %.1f against %.1f ms and p99 %.1f against %.1f ms, which bind nothing)",
				read.name, shape.name, costMilliseconds(here.p50()), head.name, costMilliseconds(before.p50()), base.name,
				ratio, predicateCostRatio, costMilliseconds(here.percentile(0.95)), costMilliseconds(before.percentile(0.95)),
				costMilliseconds(here.p99()), costMilliseconds(before.p99()))
			if float64(here.p50()) > predicateCostRatio*float64(before.p50()) {
				crossed = true
				overBound("%s over %q: p50 %.1f ms at %s is %.2f times the %.1f ms at %s, more than the %.2f bound. "+
					"Every reading at %s, in milliseconds, in the order taken: %s. Every reading at %s: %s",
					read.name, shape.name, costMilliseconds(here.p50()), head.name, ratio,
					costMilliseconds(before.p50()), base.name, predicateCostRatio,
					head.name, here.printed(), base.name, before.printed())
			}
		}
	}
	if control != "" && !crossed {
		t.Skipf("%s=%s: every read held the %.2f bound with %v dropped from %s's database, so the control did not bite; "+
			"a run with it set is no reading of c-read-cost", predicateCostControlEnv, control, predicateCostRatio,
			predicateCostInboundIndexes, head.name)
	}
	if sizes.scale != 1 {
		t.Skipf("%s=%g: this run wrote and read that share of the check's sizes, a smoke of this test whose readings are printed above "+
			"and no reading of c-read-cost", predicateCostScaleEnv, sizes.scale)
	}
}

// predicateCostStoreMigration is the migration that made the store, which every base this check
// measures against holds.
const predicateCostStoreMigration = "packages/envoy/internal/dispatch/store/migrations/20261109013400_graph_store.up.sql"

// holdsTheStore reports whether commit's tree holds the migration that made the store
// (predicateCostStoreMigration). git reads <commit>:<path> from the repository's root whatever
// directory it runs in.
func holdsTheStore(commit string) bool {
	return exec.Command("git", "cat-file", "-e", commit+":"+predicateCostStoreMigration).Run() == nil
}

// predicateCostBaseHoldsTheStore refuses a base build whose tree holds no store
// (predicateCostStoreMigration), naming the base: `git merge-base origin/main HEAD`, the base a
// branch that lands on main names in BASELINE_SHA, is such a base while the branches this child
// stacks on are unmerged, and over it every reading of a read this child changed would compare the
// store with no store. It reads the base baseBuild builds (testbaseline.Commit), and it is not
// in baseBuild, whose other callers (cybermonklabs/legion#3178's census and cost tests) measure the
// store against a base from before it.
func predicateCostBaseHoldsTheStore(t *testing.T) {
	t.Helper()
	commit := testbaseline.Commit(t)
	named := fmt.Sprintf("the commit %s=%s names", testbaseline.Variable, strings.TrimSpace(os.Getenv(testbaseline.Variable)))
	if !holdsTheStore(commit) {
		t.Fatalf("the base %s (%s) holds no migration %s (graph_store), so it is a build with no store and c-read-cost would measure nothing this child changed: set %s to the release this branch lands after, the merge base with the branch of cybermonklabs/legion#4173",
			commit, named, predicateCostStoreMigration, testbaseline.Variable)
	}
	t.Logf("the base %s (%s) holds the migration that made the store, %s", commit, named, predicateCostStoreMigration)
}
