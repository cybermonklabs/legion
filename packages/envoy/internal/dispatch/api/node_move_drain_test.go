package api

import (
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// moveDrainSpec is a spec of one criterion, which a move puts under a lens on its issue
// (moveDrainLens): Markdown shows no block under a lens, and a criterion's parent attribute names
// no lens, so the move writes no version of the spec, while the spec query's record, which takes
// the criterion with its parent, moves (R7, R14); on a converted issue the one version the move
// causes is the drain's.
const moveDrainSpec = ":::criterion{#qk1}\nThe store answers every reader.\n:::\n"

// moveDrainLens is the address of a lens alice invokes on the issue key, the parent the drain
// test moves the criterion under.
func moveDrainLens(t *testing.T, handler http.Handler, key string) string {
	t.Helper()
	lens := decodeBody[lensInvoked](t, created(t, "invoke a lens on "+key, dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+key+"/lenses", map[string]any{"on": "dispatch://" + key, "kind": "trade-off"}, "alice")))
	return graphstore.KindLens + "/" + lens.ID
}

// TestAMoveMarksItsIssuesQueriesOwedAndTheDrainWritesThem holds the move to R7's mark and drain,
// through the real route and its real drain, held at the drain's two boundaries (drainProbe) and
// read on connections of the test's own. On a converted issue whose spec holds a criterion, the
// asking agent opens an approval request on the spec query node, and alice moves the criterion
// under a lens on the issue, which changes the spec query's answer and no text:
//
//   - Once the move has committed and before the drain, the issue's issue_query_marks row stands
//     with no greatest_event, since the move writes its tuples at graphstore.NoEvent, a
//     greatest_seq no lower than the seq of the part_of the move appended, and alice as the writer
//     of the tuple there; and the spec query holds no new version.
//   - The drain then takes the issue's owner lock, then its room's lock, reads, and sends its batch
//     and COMMIT (the server's trace); once it has committed and before the route publishes, nothing
//     is published yet, the spec query stands at one version more, whose artifact.version names
//     alice, the approval request on the query node names that version, and the mark is gone. The
//     drained version holds the version_renders row of its record.
//   - A move on an issue that has not converted marks nothing, runs no drain and writes no version.
//
// The probe's document service settles after an hour, as TestIssueQueryMarksDrainAfterCommit's does,
// so no settlement writes a version while the test reads them.
func TestAMoveMarksItsIssuesQueriesOwedAndTheDrainWritesThem(t *testing.T) {
	handler, database, deps, probe, sent := newDrainProbeServer(t)
	createReferenceAPIProject(t, handler, "MVQ")
	issue := createDigestIssue(t, handler, database, "MVQ", "The drained move", moveDrainSpec, false)
	key := issue.Key
	criterion := readDecisionDocument(t, database, key, "spec").address("qk1")
	lens := moveDrainLens(t, handler, key)
	specQuery := queryArtifactOf(t, database, key, graphstore.QuerySpec)
	request := requestApproval(t, handler, specQuery)
	published, stop := deps.Events.Subscribe()
	defer stop()
	t.Cleanup(func() { probe.before, probe.after = nil, nil })

	before := newestQueryVersion(t, database, specQuery)
	held, tag := placementOf(t, database, "block", criterion)
	publishedSince(published)
	from := len(sent.Statements())
	var committed, drained bool
	probe.before = func(drainedKey string) {
		committed = true
		if drainedKey != key {
			t.Errorf("the move drains %s, want the issue it moved a node of, %s", drainedKey, key)
		}
		mark, marked := readQueryMarkRow(t, database, key)
		placed, _ := placementOf(t, database, "block", criterion)
		switch {
		case !marked:
			t.Errorf("the move of %s committed and left no issue_query_marks row of %s", criterion, key)
		case mark.event != nil:
			t.Errorf("the move's mark of %s holds greatest_event %d, want none: the move writes its tuples at graphstore.NoEvent", key, *mark.event)
		case mark.seq < placed.Seq:
			t.Errorf("the move's mark of %s holds greatest_seq %d, want at least %d, the seq of the part_of it appended", key, mark.seq, placed.Seq)
		case mark.writer.ID != "alice":
			t.Errorf("the tuple at the move's mark's greatest_seq %d was written by %+v, want alice", mark.seq, mark.writer)
		}
		if version := newestQueryVersion(t, database, specQuery); version.number != before.number {
			t.Errorf("the spec query of %s stands at version %d once the move committed and before its drain, want %d", key, version.number, before.number)
		}
	}
	probe.after = func(drainedKey string, appended []model.Event) {
		drained = true
		if early := publishedSince(published); len(early) > 0 {
			t.Errorf("%s published locally before the move's drain committed", early[0].Type)
		}
		version := newestQueryVersion(t, database, specQuery)
		if version.number != before.number+1 {
			t.Errorf("the move's drain left the spec query of %s at version %d, want %d", key, version.number, before.number+1)
		}
		if _, actor, held := documentEventOf(t, database, "artifact.version", specQuery, version.number); !held {
			t.Errorf("version %d of the spec query, the move's drain's, has no artifact.version", version.number)
		} else if actor.ID != "alice" {
			t.Errorf("the artifact.version of the move's drained version names %+v, want alice, who wrote the tuple at the mark's greatest_seq", actor)
		}
		if named := requestedVersionOf(t, database, request.ID, specQuery); named != version.number {
			t.Errorf("the request on %s names version %d after the drain, want %d", specQuery, named, version.number)
		}
		requireNoQueryMark(t, database, "at the publish of the move's drain", drainedKey)
		if !slices.ContainsFunc(appended, func(event model.Event) bool { return event.Type == "artifact.version" }) {
			t.Errorf("the move's drain answers %+v for the route to publish, want its artifact.version among them", appended)
		}
	}
	response := patchNode(t, handler, key, criterion, tag, map[string]any{"parent": lens}, "alice")
	probe.before, probe.after = nil, nil
	if response.Code != http.StatusOK {
		t.Fatalf("the move of %s under %s answered %d %s, want 200", criterion, lens, response.Code, response.Body.String())
	}
	if !committed || !drained {
		t.Fatalf("the move ran no drain of %s once it committed (before %t, after %t)", key, committed, drained)
	}
	if moved, _ := placementOf(t, database, "block", criterion); storeAddress(moved.ToKind, moved.ToID) != lens || moved.Seq == held.Seq {
		t.Errorf("the move of %s left it under %s at placement_seq %d, want %s at a new one", criterion, storeAddress(moved.ToKind, moved.ToID), moved.Seq, lens)
	}
	if blind := sent.Blind(); len(blind) > 0 {
		t.Fatalf("the statement recorder could not read %q", blind)
	}
	requireDrainTrace(t, "the move of "+criterion, specQuery, sent.Statements()[from:])
	requireRecordedDigest(t, database, "the drained version of the move", key, graphstore.QuerySpec, newestQueryVersion(t, database, specQuery))

	unconverted := createDigestIssue(t, handler, database, "MVQ", "A move on an issue that has not converted", moveDrainSpec, true)
	unconvertedCriterion := readDecisionDocument(t, database, unconverted.Key, "spec").address("qk1")
	unconvertedLens := moveDrainLens(t, handler, unconverted.Key)
	versions := issueVersionCounts(t, database.Pool, unconverted.Key)
	drains := probe.drains.Load()
	_, unconvertedTag := placementOf(t, database, "block", unconvertedCriterion)
	if moved := patchNode(t, handler, unconverted.Key, unconvertedCriterion, unconvertedTag, map[string]any{"parent": unconvertedLens}, "alice"); moved.Code != http.StatusOK {
		t.Fatalf("the move of %s under %s answered %d %s, want 200", unconvertedCriterion, unconvertedLens, moved.Code, moved.Body.String())
	}
	requireNoQueryMark(t, database, "after a move on an issue that has not converted", unconverted.Key)
	if ran := probe.drains.Load(); ran != drains {
		t.Errorf("a move on %s, which has not converted, ran %d drains, want none", unconverted.Key, ran-drains)
	}
	if after := issueVersionCounts(t, database.Pool, unconverted.Key); !maps.Equal(after, versions) {
		t.Errorf("a move on %s, which has not converted, leaves its documents at the versions %v, want %v", unconverted.Key, after, versions)
	}
}
