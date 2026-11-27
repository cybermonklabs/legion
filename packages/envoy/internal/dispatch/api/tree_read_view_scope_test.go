package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAViewReadsTheRowsThatPlaceItsNodes holds a view's head read (refs.HeadViewRows), which of the
// blocks under the issue's node reads the first alone and follows the moved_to tuples of the
// blocks its comments on text are on, to the walk of the whole tree (headRig.after), in three
// states no state of TestAViewReadOfItsRowsAnswersAsTheWalk holds: a spec that opens with a
// callout, the issue's memo, which the spec view takes; a spec whose callout follows a paragraph,
// so it is no memo, which a view that read the callout and not that paragraph would take for one;
// and a person's comment whose anchor names a block that is no node of the tree, which its moved_to
// carries to a paragraph of a list of the spec, so the comment stands with the issue's node, which
// the plan view answers.
func TestAViewReadsTheRowsThatPlaceItsNodes(t *testing.T) {
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, deps, _ := newRegisteredTestServer(t, testServerOptions{store: traced, settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	settle := func(key string) {
		t.Helper()
		settleIssue(t, documents, key)
		reconcileBlockIndexPass(t, deps)
	}
	callout := func(id, words string) string {
		return ":::callout{#" + id + " kind=\"note\" title=\"\"}\n" + words + "\n:::\n\n"
	}
	const requirement = ":::requirement{#r1}\nThe list orders the asks by urgency.\n:::\n"

	memo := createInteractionIssue(t, handler, "VSC", "A spec that opens with its memo", callout("m1", "The memo of the issue.")+requirement).Key
	settle(memo)
	r := newHeadRig(t, handler, database, counter, memo)
	r.requireMarked(memo)
	r.after("a spec that opens with a callout", memo)
	if taken := treeRead(t, handler, memo, "?view=spec", "alice", true).nodesNamed(refs.BlockNodeKind,
		readDecisionDocument(t, database, memo, "spec").address("m1")); len(taken) != 1 || taken[0].Role != treeRoleQuery {
		t.Errorf("the spec view of %s answers its opening callout as %+v, want it once, as the memo its query takes", memo, taken)
	}

	issue := func(title, spec string) string {
		t.Helper()
		key := decodeBody[struct {
			Key string `json:"key"`
		}](t, created(t, "the issue "+title, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": "VSC", "title": title, "spec": spec,
		}, "alice"))).Key
		r.requireMarked(key)
		settle(key)
		return key
	}
	late := issue("A spec whose callout follows a paragraph", "An opening paragraph.\n\n"+callout("m2", "A callout that is no memo.")+requirement)
	r.after("a spec whose callout follows a paragraph", late)
	if taken := treeRead(t, handler, late, "?view=spec", "alice", true).nodesNamed(refs.BlockNodeKind,
		readDecisionDocument(t, database, late, "spec").address("m2")); len(taken) != 0 {
		t.Errorf("the spec view of %s answers the callout after its opening paragraph as %+v, want none: the memo is the issue's first child alone", late, taken)
	}

	moved := issue("A comment whose anchor moved", requirement+"\n- A listed line to talk about.\n- Another listed line.\n")
	uploadDocument(t, handler, "/api/v1/issues/"+moved, "plan.md", ":::plan_task{#p1}\nBuild the index first.\n:::\n")
	comment := createThreadComment(t, handler, moved, map[string]any{
		"body": "Which line?", "anchor": map[string]any{"artifact": "spec", "quote": "A listed line to talk about"},
	}, "alice").ID
	settle(moved)
	r.after("a comment on a listed line", moved)
	listed := blockWithText(t, database, "A listed line to talk about")
	away := readDecisionDocument(t, database, moved, "spec").address("vsc-moved-away")
	r.releaseBefore("a comment whose anchor moved to a listed line", moved, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
			select distinct t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id, clock_timestamp(), '`+headWho+`'::jsonb, 0::bigint, false
			from graph_tuples t
			where t.from_kind = $1::text and t.from_id = $2::text and t.kind in ($3::text, $4::text) and t.asserted
			union all
			select $1::text, $2::text, held.kind, $5::text, $6::text, clock_timestamp(), '`+headWho+`'::jsonb, 0::bigint, true
			from unnest(array[$3::text, $4::text]) held (kind)
			union all
			select $5::text, $6::text, $7::text, $5::text, $8::text, clock_timestamp(), '`+headWho+`'::jsonb, 0::bigint, true`,
			graphstore.KindComment, comment, graphstore.TupleAnchoredIn, graphstore.TuplePartOf, refs.BlockNodeKind, away,
			graphstore.TupleMovedTo, listed)
		return err
	})
	if carried := treeRead(t, handler, moved, "?view=plan", "alice", true).nodesNamed(graphstore.KindComment, comment); len(carried) != 1 {
		t.Errorf("the plan view of %s answers the comment whose anchor moved to %s as %+v, want it once, with the issue's node it stands with", moved, listed, carried)
	}
}
