//go:build !race

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The phases of a write of the node routes the xid window test tells apart, each by the
// statements of its committed transaction (nodeRoutePhaseOf): a move whose text Markdown cannot
// show, which writes its tuples alone; a move whose text moves, which writes a version of its
// document under the room's lock in the same transaction (R7); a delete of a block, which takes
// the block out of its document's text and so writes a version of that document in the same
// transaction too (R7); the fill of that version's record digest, which its batch left owed, in a
// transaction of its own once the write has committed (R15, docs' fillOwedDigests); and the drain
// of the queries the write marked owed, which writes their versions in a transaction of its own
// after that (docs.Service.DrainOwedQueries).
const (
	phaseFlatMove    = "flat-move"
	phaseVersionMove = "move-version"
	phaseNodeDelete  = "node-delete"
	phaseFill        = "fill"
	phaseDrain       = "drain"
	// phaseUnclassified names a window whose statements hold the statement of no phase.
	phaseUnclassified = "unclassified"
)

// The statement each phase is told by, each written by one place of the server alone, so a
// window that holds one holds that phase's write and no other's: a drain by sqlClearsQueryMark and
// a fill by sqlFillsDigest (write_batch_test.go, which every build of the package's tests holds),
// and the others by these. A delete and a move that each write a version both insert it, so
// neither of them is told by that insert: a delete is told by the delete of the blocks it takes
// out of the index, a move by the read of the placement_seq it answers, which no delete reads.
const (
	// sqlRemovesBlocks deletes the rows of the blocks a version no longer holds, in the index
	// pass of the version a delete writes (refs' BlockIndexing.queueRows): no move takes a block
	// out of its document.
	sqlRemovesBlocks = "delete from document_blocks"
	// sqlReadsPlacement reads the placement_seq the new part_of of a move took, which every move
	// that reparents a node queues (queueNodeMove) and no delete does.
	sqlReadsPlacement = "select max(t.seq) from graph_tuples"
	// sqlInsertsVersion is the insert of a document version (docs' versionInsert), which a move
	// whose text moves writes beside its tuples and a flat move does not.
	sqlInsertsVersion = "insert into artifact_versions"
	// sqlRecordsCarry records a review carried to a version in the batch that writes the version
	// (docs' reviewWrite.queueEvent): an approval carried, not merely weighed.
	sqlRecordsCarry = "insert into artifact_review_carries"
	// sqlRepinsCarry repins that carried review to the record digest the fill read
	// (docs' queueFilledDigest): the fill's alone.
	sqlRepinsCarry = "update artifact_review_carries set record_sha256"
)

// nodeRouteDecision is the decision and the reading it closes with that the fixture's spec holds
// after its requirement, written as the reading tests' spec writes them (readingSpec). The
// reading is the whole change a delete makes that the approval carry's reading case carries an
// approval across (docs.readingCase): a person's approval of the spec stands at the version the
// delete writes.
const nodeRouteDecision = "::::decision{#xd1}\n" + readingDecisionOne + "\n\n:::reading{#xr1}\n" + readingOfOne + "\n:::\n::::\n"

// nodeRouteStatements is the statements of window's transaction, each lowered and with its
// whitespace folded, so a statement is found by the words its source writes whatever its layout.
func nodeRouteStatements(window storetest.XidWindow) string {
	var sql strings.Builder
	for _, statement := range window.Statements {
		sql.WriteString(strings.Join(strings.Fields(strings.ToLower(statement.SQL)), " "))
		sql.WriteByte('\n')
	}
	return sql.String()
}

// nodeRoutePhaseOf is the phase of the committed transaction whose statements are sql
// (nodeRouteStatements): the one phase whose statement it holds. A window that holds the statements
// of two phases - a write that ran its fill inside its own transaction - is named by both, joined
// by "+", and one that holds none is phaseUnclassified, so neither is told as a phase it is not.
func nodeRoutePhaseOf(sql string) string {
	var phases []string
	if strings.Contains(sql, sqlClearsQueryMark) {
		phases = append(phases, phaseDrain)
	}
	if strings.Contains(sql, sqlFillsDigest) {
		phases = append(phases, phaseFill)
	}
	if strings.Contains(sql, sqlRemovesBlocks) {
		phases = append(phases, phaseNodeDelete)
	}
	if strings.Contains(sql, sqlReadsPlacement) {
		if strings.Contains(sql, sqlInsertsVersion) {
			phases = append(phases, phaseVersionMove)
		} else {
			phases = append(phases, phaseFlatMove)
		}
	}
	switch len(phases) {
	case 0:
		return phaseUnclassified
	case 1:
		return phases[0]
	}
	return strings.Join(phases, "+")
}

// nodeRouteWrite is one measured request of the node routes' test, built with what the test read
// before the measurement (the placement_seq it names, the count a dry run answered), and the check
// of what it left.
type nodeRouteWrite struct {
	request func(http.Handler) *httptest.ResponseRecorder
	// after fails its test unless the request left the store as it meant to, read once it has
	// answered; nil for a request with nothing to read beyond its answer.
	after func(t *testing.T)
}

// nodeRouteStep is one row of the node routes' test: the request it measures (operation and name
// tell it in the log), the phases of the transactions the request commits, in commit order, and
// whether the write carries a person's approval to the version it writes. ready reads what the
// request names and answers the request, before any measurement starts.
type nodeRouteStep struct {
	name, operation string
	phases          []string
	carries         bool
	ready           func(t *testing.T) nodeRouteWrite
}

// digestWords is a record digest as a failure names it: the digest, or "none" where none is held.
func digestWords(digest *string) string {
	if digest == nil {
		return "none"
	}
	return *digest
}

// requireCarriedReview fails t unless the spec holds, one version after approved, the review the
// delete carried alice's approval with: approved, in her name byte for byte, with the reading
// case's reason; a row of artifact_review_carries that names approved's version as its source and,
// once the fill has run, the record digest it repinned that row to - the digest the version's own
// version_renders row holds, which owes none. A window that merely held the carry's statement
// would show the approval invoked; this shows it carried, and the fill's repin of it.
func requireCarriedReview(t *testing.T, f *xidFixture, approved model.ArtifactReview) {
	t.Helper()
	var approval, carried *servedReview
	reviews := documentReviews(t, f.handler, f.specID)
	for index := range reviews {
		switch review := &reviews[index]; {
		case review.State != "approved":
		case review.Version == approved.Version && approval == nil:
			approval = review
		case review.Version > approved.Version && carried == nil:
			carried = review
		}
	}
	if approval == nil || carried == nil {
		t.Fatalf("the spec holds %d reviews, want alice's approval of version %d and the review the delete carried it with", len(reviews), approved.Version)
	}
	if carried.Version != approved.Version+1 {
		t.Errorf("the carried review names version %d, want %d, the one version the delete wrote", carried.Version, approved.Version+1)
	}
	if carried.Reason == nil || *carried.Reason != readingCarrySentence(approved.Version) {
		t.Errorf("the carried review's reason is %v, want %q", carried.Reason, readingCarrySentence(approved.Version))
	}
	if string(carried.Actor) != string(approval.Actor) {
		t.Errorf("the carried review's actor is %s, want the approval's %s", carried.Actor, approval.Actor)
	}
	if from, held := carriedFromRow(t, f.database, carried.ID); !held || from != approved.Version {
		t.Errorf("the carried review holds %s, want the artifact_review_carries row naming version %d", carriedFromRowWords(from, held), approved.Version)
	}
	var pinned *string
	if err := f.database.Pool.QueryRow(context.Background(),
		`select record_sha256 from artifact_review_carries where review_id = $1::uuid`, carried.ID).Scan(&pinned); err != nil {
		t.Fatalf("read the record digest the carried review %s was repinned to: %v", carried.ID, err)
	}
	row := versionFillRowOf(t, f.database, f.specID, carried.Version)
	if row.owed || row.digest == nil || pinned == nil || *pinned != *row.digest {
		t.Errorf("the carried review of version %d is pinned to the record digest %s, want the %s its version_renders row holds, which owes none (owed %t)",
			carried.Version, digestWords(pinned), digestWords(row.digest), row.owed)
	}
	t.Logf("the delete carried alice's approval of version %d to version %d, review %s, and the fill repinned it to the record digest of that version", approved.Version, carried.Version, carried.ID)
}

// TestXidWindowsOfTheNodeRoutesTheDrainAndTheFill measures, on the large spec of the xid fixture
// with a criterion, a requirement and a decision with its reading after it, the windows of the
// node routes' writes that change a query and of the fill and the drain each runs (R7, R15,
// c-windows-kept), in four steps, each on the state the step before it left:
//
//   - PATCH, a flat move: the requirement moved under a lens on the issue, which the requirement's
//     parent attribute cannot name, so its text and its document stay as they are while the spec
//     query's record moves. Its transactions: flat-move, drain.
//   - PATCH, a move that writes a version: the requirement moved under the criterion, whose parent
//     attribute the text then gains under the room's lock, and whose version's record digest the
//     fill records after the COMMIT. Its transactions: move-version, fill, drain.
//   - DELETE, a delete that writes a version: the requirement taken out of the spec, which
//     changes the spec query's record and writes the spec's version in the same transaction, a
//     version of the document whose text lost a block, which its fill and the drain follow. Its
//     transactions: node-delete, fill, drain, and no review carried, since no approval stands.
//   - DELETE, a delete that carries an approval: alice approves the spec, and the reading is taken
//     out of the decision, which changes the decisions query's record and writes the spec's
//     version, whose only change from the approved one is the reading, so the approval carries
//     (docs.readingCase). Its transactions: node-delete, whose batch records the carried review,
//     then fill, which repins that review to the record digest, then drain; the review itself is
//     read back (requireCarriedReview).
//
// Each request's committed transactions are told apart by their statements (nodeRoutePhaseOf),
// each named with its operation, its phase and its window, and each held to
// storetest.XidBatchedWindow: its reads, then one batch and its COMMIT. Each step requires its
// phases exactly, in this order, each in a transaction of its own and no other: a write that ran
// its fill or its version in a transaction of its own after its COMMIT would show two windows for
// one write, and one that ran the fill inside its own would show a window held by two phases.
func TestXidWindowsOfTheNodeRoutesTheDrainAndTheFill(t *testing.T) {
	// Not parallel: the xid rig measures only in a test that runs alone in the process.
	large, _ := xidSpec(1)
	f := newXidFixture(t, testServerOptions{}, large+"\n:::criterion{#k1}\nThe store answers in time.\n:::\n\n"+
		":::requirement{#m1}\nEvery read answers in a second.\n:::\n\n"+nodeRouteDecision)
	lens := decodeBody[lensInvoked](t, created(t, "invoke a lens on the issue", dispatchRequest(t, f.handler, http.MethodPost,
		"/api/v1/issues/"+f.issueKey+"/lenses", map[string]any{"on": "dispatch://" + f.issueKey, "kind": "trade-off"}, "alice")))
	requirement := blockAddressOf(t, f.database, f.issueKey, "m1")
	criterion := blockAddressOf(t, f.database, f.issueKey, "k1")
	reading := blockAddressOf(t, f.database, f.issueKey, "xr1")

	const (
		patchOperation  = "PATCH /api/v1/issues/{key}/nodes/{id}"
		deleteOperation = "DELETE /api/v1/issues/{key}/nodes/{id}"
	)
	// moveUnder is the move of the requirement under parent, naming the placement_seq the
	// requirement stands at when the step is ready.
	moveUnder := func(parent string) func(t *testing.T) nodeRouteWrite {
		return func(t *testing.T) nodeRouteWrite {
			_, tag := placementOf(t, f.database, "block", requirement)
			return nodeRouteWrite{request: func(h http.Handler) *httptest.ResponseRecorder {
				return patchNode(t, h, f.issueKey, requirement, tag, map[string]any{"parent": parent}, "alice")
			}}
		}
	}
	// remove is the delete of node by alice, naming the placement_seq node stands at and the count
	// the dry run answers, as a confirm does; once it has answered, node must read gone.
	remove := func(node string) func(t *testing.T) nodeRouteWrite {
		return func(t *testing.T) nodeRouteWrite {
			_, tag := placementOf(t, f.database, "block", node)
			count := deletedCount(t, "the dry run of "+node, deleteNodeAs(personCaller(t, f.handler, "alice"), f.issueKey, node, true, -1, ""))
			return nodeRouteWrite{
				request: func(h http.Handler) *httptest.ResponseRecorder {
					return deleteNodeAs(personCaller(t, h, "alice"), f.issueKey, node, false, count, tag)
				},
				after: func(t *testing.T) {
					if state := nodeStateOf(t, f.database, "block", node); state != graphstore.StateGone {
						t.Errorf("after the delete of %s it reads %s, want %s", node, state, graphstore.StateGone)
					}
				},
			}
		}
	}
	// approvedThenRemove is remove of node once alice has approved the spec as it stands, which no
	// write has left owed a drain or a fill (the step before it ran both).
	var approved model.ArtifactReview
	approvedThenRemove := func(node string) func(t *testing.T) nodeRouteWrite {
		return func(t *testing.T) nodeRouteWrite {
			approved = reviewDocument(t, f.handler, f.specID, "alice", "approved", "")
			return remove(node)(t)
		}
	}

	moveFlow := []string{phaseFlatMove, phaseDrain}
	versionFlow := []string{phaseVersionMove, phaseFill, phaseDrain}
	deleteFlow := []string{phaseNodeDelete, phaseFill, phaseDrain}
	steps := []nodeRouteStep{
		{name: "flat move", operation: patchOperation, phases: moveFlow, ready: moveUnder(graphstore.KindLens + "/" + lens.ID)},
		{name: "move that writes a version", operation: patchOperation, phases: versionFlow, ready: moveUnder(criterion)},
		{name: "delete that writes a version", operation: deleteOperation, phases: deleteFlow, ready: remove(requirement)},
		{name: "delete that carries an approval", operation: deleteOperation, phases: deleteFlow, carries: true, ready: approvedThenRemove(reading)},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			awaitQuietDatabase(t, f.database, false)
			write := step.ready(t)
			// What the step read and wrote to be ready - the dry run, the approval - is done before
			// the request is measured, so the measurement holds the request's own transactions alone.
			awaitQuietDatabase(t, f.database, false)
			report := f.measure(t, http.StatusOK, storetest.XidBatchedWindow, write.request)
			var phases []string
			for index, window := range report.Windows {
				sql := nodeRouteStatements(window)
				phase := nodeRoutePhaseOf(sql)
				phases = append(phases, phase)
				t.Logf("%s, %s: phase %s: %s", step.operation, step.name, phase, window.Summary())
				for _, broken := range window.Violations() {
					t.Errorf("%s, %s: phase %s: xid window violation: %s", step.operation, step.name, phase, broken)
				}
				// The carry is recorded in the batch of the write's own transaction, the first, and
				// repinned in the fill's: no other window of the request may hold either statement.
				if want := step.carries && index == 0; strings.Contains(sql, sqlRecordsCarry) != want {
					t.Errorf("%s, %s: phase %s: the window records a carried review: %t, want %t",
						step.operation, step.name, phase, !want, want)
				}
				if want := step.carries && phase == phaseFill; strings.Contains(sql, sqlRepinsCarry) != want {
					t.Errorf("%s, %s: phase %s: the window repins a carried review: %t, want %t",
						step.operation, step.name, phase, !want, want)
				}
			}
			if !slices.Equal(phases, step.phases) {
				t.Errorf("%s, %s: the request's transactions were %v, want %v: each phase in a transaction of its own, in this order",
					step.operation, step.name, phases, step.phases)
			}
			if write.after != nil {
				write.after(t)
			}
			if step.carries {
				requireCarriedReview(t, f, approved)
			}
		})
	}
}
