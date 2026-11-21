package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// requestForRun is dispatchRequest with the run named in contracts.GoingRunHeader, as a write
// made for that run sends it (GoingCaller.Do). A nil body sends none, as dispatchRequest sends
// none for a DELETE or a follow.
func requestForRun(t *testing.T, handler http.Handler, method, target string, body any, login, run string) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			t.Fatalf("encode request body: %v", err)
		}
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", login)
	request.Header.Set(contracts.GoingRunHeader, run)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// runWindowAhead moves the run's window an hour ahead in the test's own database, so its
// window never decides whether a write made for it is accepted.
func runWindowAhead(t *testing.T, database *store.Store, run string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update going_requests set ends_at = now() + interval '1 hour' where id = $1::uuid`, run); err != nil {
		t.Fatalf("move run %s's window ahead: %v", run, err)
	}
}

// changedByRunCount is how many changed_by_run tuples out of the node point at the run.
func changedByRunCount(t *testing.T, database *store.Store, kind, id, run string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
	`, kind, id, graphstore.TupleChangedByRun, graphstore.KindInteraction, run).Scan(&count); err != nil {
		t.Fatalf("count the changed_by_run of %s/%s to run %s: %v", kind, id, run, err)
	}
	return count
}

// tuplesIntoRun is how many tuples of any node point at the run's interaction.
func tuplesIntoRun(t *testing.T, database *store.Store, run string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples where to_kind = $1 and to_id = $2
	`, graphstore.KindInteraction, run).Scan(&count); err != nil {
		t.Fatalf("count the tuples into run %s: %v", run, err)
	}
	return count
}

// runReads is what one person reads of the issue and of the first person's person node, kept to
// compare before and after the run: the references of the issue in each direction, as
// edgeEnds names them, the backlink count the issue read carries, and the backlink count of the
// person node. bodies is every answer body read, for the words of the run to be looked for in.
type runReads struct {
	to, from    []string
	issueCount  int
	personCount int
	bodies      []string
}

// TestTheReferencesReadAnswersARunToThePersonWhoseRunItWas is c-interaction-page
// (spec requirement r-interaction). Two people read the issue's references both
// ways, its backlink count and the backlink count of alice's person node. Alice's run is recorded
// with two frames; under the run's header and with her credential a comment is written on the
// issue, the issue's labels are changed through its PATCH, an open ask of the issue is answered,
// a second is followed, and the words of a third and of a second comment are edited; and the run
// is ended. Alice then reads, of the comment, of the issue, of each of the three asks and of the
// second comment, one changed_by_run to the interaction of her run, which answers its kind, her
// words cut at the 240 runes an edge's excerpt keeps and its own address. At that address, read
// both ways, the interaction answers her words whole as its text, its one authored_by to her
// person node and its frames in sequence, the two it appended and then the terminal one. At the
// far end of that authored_by, read into her person node (the read behind ?to=), the run answers
// her words cut at 240 runes and no frame. The issue's count moves alike for both, since each
// tuple points away from the node the run changed, her person node's count is one more for her,
// its authored_by, and bob reads nothing of the run anywhere. Last, under the same run, she
// removes an untouched document an earlier run of hers made, which holds that earlier run's
// changed_by_run, and a document is made again under its title with no run header, which takes
// the removed document's node: neither document's references hold a changed_by_run, to either
// run.
func TestTheReferencesReadAnswersARunToThePersonWhoseRunItWas(t *testing.T) {
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	server := directServer(deps)
	ctx := context.Background()
	issue := createTestIssue(t, handler, "RUN", "The issue a run writes on")
	issueRef := "dispatch://" + issue
	const alice, bob = "alice", "bob"
	// Longer than an edge's excerpt keeps, and with a rune of three bytes in its head, so the cut
	// and the whole are told apart rune for rune.
	const words = "Which plan holds the rollback step of the cutover — the step that moves every reader of the" +
		" store back to the tables it read before, in the order the deploy section gives, with the census" +
		" run after each move and the backfill run once the roll is done, so that nobody reads a node the" +
		" older build cannot address?"
	whole := []rune(words)
	if len(whole) <= 240 {
		t.Fatalf("the run's words are %d runes, want more than the 240 an excerpt keeps, so the cut proves something", len(whole))
	}
	excerpt := string(whole[:240])

	read := func(login string) runReads {
		t.Helper()
		var kept runReads
		for _, direction := range []string{"to", "from"} {
			response := referencesAt(t, handler, direction, issueRef, login)
			t.Logf("%s's ?%s=%s: status=%d body=%s", login, direction, issueRef, response.Code, response.Body.String())
			if response.Code != http.StatusOK {
				t.Fatalf("%s's ?%s= of the issue answers %d %s", login, direction, response.Code, response.Body.String())
			}
			kept.bodies = append(kept.bodies, response.Body.String())
			ends := edgeEnds(decodeBody[model.GraphReferences](t, response).Edges)
			if direction == "to" {
				kept.to = ends
			} else {
				kept.from = ends
			}
		}
		issueRead := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue, nil, login)
		if issueRead.Code != http.StatusOK {
			t.Fatalf("%s's read of the issue answers %d %s", login, issueRead.Code, issueRead.Body.String())
		}
		kept.bodies = append(kept.bodies, issueRead.Body.String())
		kept.issueCount = decodeBody[struct {
			ReferencedByCount int `json:"referenced_by_count"`
		}](t, issueRead).ReferencedByCount
		counts, err := refs.BacklinkCounts(ctx, database.Pool, model.Actor{Kind: "user", ID: login}, graphstore.KindPerson, []string{alice}, nil)
		if err != nil {
			t.Fatalf("count the backlinks of alice's person node as %s: %v", login, err)
		}
		kept.personCount = counts[alice]
		t.Logf("%s reads the issue's backlink count %d and alice's person node's %d", login, kept.issueCount, kept.personCount)
		return kept
	}
	// What the run changes besides, each written for no run, so each changed_by_run it then holds
	// is the run's change of it: three open asks a session opened on the issue and a second
	// comment of alice's. All of it is written before the first read, so what each person's
	// counts move by is what the run did.
	answered := createIssueAsk(t, handler, issue, "Does the rollback step stay in the plan?")
	followed := createIssueAsk(t, handler, issue, "Does the census run after each move?")
	reworded := createIssueAsk(t, handler, issue, "Does the backfill run once the roll is done?")
	remark := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
		map[string]any{"body": "A second remark, written for no run."}, alice)
	if remark.Code != http.StatusCreated {
		t.Fatalf("write the second comment with no run header: status=%d body=%s", remark.Code, remark.Body.String())
	}
	second := decodeBody[writtenRecord](t, remark).ID

	before := map[string]runReads{alice: read(alice), bob: read(bob)}

	run, err := server.recordGoingRequest(ctx, model.Actor{Kind: "user", ID: alice}, "", words)
	if err != nil {
		t.Fatalf("record alice's run: %v", err)
	}
	runWindowAhead(t, database, run.ID)
	frames := []going.Event{going.Text("The rollback "), going.Text("is in the plan.")}
	for index, frame := range frames {
		if sequence, err := appendGoingEvent(ctx, database.Pool, run.ID, frame); err != nil || sequence != index+1 {
			t.Fatalf("append frame %d of the run: sequence %d, %v", index+1, sequence, err)
		}
	}
	written := requestForRun(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
		map[string]any{"body": "The run's comment on the issue."}, alice, run.ID)
	if written.Code != http.StatusCreated {
		t.Fatalf("write the comment under the run's header: status=%d body=%s", written.Code, written.Body.String())
	}
	comment := decodeBody[writtenRecord](t, written).ID
	// The issue's own field write, under the same run: its labels.
	labels := []string{"run-changed"}
	patched := requestForRun(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"labels": labels}, alice, run.ID)
	t.Logf("alice's PATCH of the issue's labels under the run: status=%d body=%s", patched.Code, patched.Body.String())
	if patched.Code != http.StatusOK {
		t.Fatalf("change the issue's labels under the run's header: status=%d body=%s", patched.Code, patched.Body.String())
	}
	if got := decodeBody[model.Issue](t, patched).Labels; !slices.Equal(got, labels) {
		t.Fatalf("the PATCH under the run left the issue's labels %v, want %v", got, labels)
	}
	// Under the same run she answers the first ask, puts a session on the second's followers,
	// and edits the words of the third and of her second comment.
	for _, write := range []struct {
		what, method, target string
		body                 any
		status               int
	}{
		{"answer the ask " + answered, http.MethodPost, "/api/v1/asks/" + answered + "/answer",
			map[string]any{"selected": []string{"Yes"}, "expected_edited_at": nil}, http.StatusOK},
		{"follow the ask " + followed, http.MethodPut, "/api/v1/asks/" + followed + "/followers/run-follower", nil, http.StatusNoContent},
		{"edit the words of the ask " + reworded, http.MethodPatch, "/api/v1/asks/" + reworded,
			map[string]any{"question": "Does the backfill run once, after the roll is done?"}, http.StatusOK},
		{"edit the words of the comment " + second, http.MethodPatch, "/api/v1/comments/" + second,
			map[string]any{"body": "A second remark, its words edited for the run."}, http.StatusOK},
	} {
		response := requestForRun(t, handler, write.method, write.target, write.body, alice, run.ID)
		t.Logf("alice's %s under run %s: status=%d body=%s", write.what, run.ID, response.Code, response.Body.String())
		if response.Code != write.status {
			t.Fatalf("%s under run %s's header: status=%d body=%s, want %d", write.what, run.ID, response.Code, response.Body.String(), write.status)
		}
	}
	terminal := going.End(6, 0)
	if _, err := endGoingRun(ctx, database.Pool, run.ID, terminal); err != nil {
		t.Fatalf("end the run: %v", err)
	}
	frames = append(frames, terminal)
	commentRef := "dispatch://" + issue + "/comment/" + comment
	interactionRef := "dispatch://interaction/" + run.ID
	// Each node the run changed: the comment it wrote, the issue whose labels it changed, the ask
	// it answered, the ask it followed, the ask and the comment whose words it edited.
	changedNodes := []struct{ what, kind, id, ref string }{
		{"comment", graphstore.KindComment, comment, commentRef},
		{"issue", graphstore.KindIssue, issue, issueRef},
		{"answered ask", graphstore.KindAsk, answered, issueRef + "/ask/" + answered},
		{"followed ask", graphstore.KindAsk, followed, issueRef + "/ask/" + followed},
		{"edited ask", graphstore.KindAsk, reworded, issueRef + "/ask/" + reworded},
		{"edited comment", graphstore.KindComment, second, issueRef + "/comment/" + second},
	}

	// Each node the run changed is read both ways by both people. Alice reads its one
	// changed_by_run out of it to her run's interaction, with the head of her words as the
	// excerpt, and nothing of the run into it; bob reads no changed_by_run and no interaction
	// either way.
	after := map[string]runReads{}
	for _, login := range []string{alice, bob} {
		after[login] = read(login)
		for _, changed := range changedNodes {
			for _, direction := range []string{"to", "from"} {
				response := referencesAt(t, handler, direction, changed.ref, login)
				t.Logf("%s's ?%s=%s: status=%d body=%s", login, direction, changed.ref, response.Code, response.Body.String())
				if response.Code != http.StatusOK {
					t.Fatalf("%s's ?%s= of the %s %s answers %d %s", login, direction, changed.what, changed.id, response.Code, response.Body.String())
				}
				reads := after[login]
				reads.bodies = append(reads.bodies, response.Body.String())
				after[login] = reads
				edges := decodeBody[model.GraphReferences](t, response).Edges
				byRun := slices.DeleteFunc(slices.Clone(edges), func(edge model.GraphEdge) bool {
					return edge.Kind != graphstore.TupleChangedByRun && edge.Node.Kind != graphstore.KindInteraction
				})
				switch {
				case login == bob && len(byRun) != 0:
					t.Errorf("bob's ?%s= of the %s %s answers %+v, want no changed_by_run and no interaction, of run %s or any other",
						direction, changed.what, changed.id, byRun, run.ID)
				case login == alice && direction == "to" && len(byRun) != 0:
					t.Errorf("alice's ?to= of the %s %s answers %+v: the changed_by_run to run %s points away from it",
						changed.what, changed.id, byRun, run.ID)
				case login == alice && direction == "from":
					if len(byRun) != 1 || byRun[0].Kind != graphstore.TupleChangedByRun || byRun[0].Direction != "out" {
						t.Fatalf("alice's references of the %s %s hold %+v, want its one changed_by_run to the interaction of run %s",
							changed.what, changed.id, byRun, run.ID)
					}
					node := byRun[0].Node
					if node.Kind != graphstore.KindInteraction || node.ID != run.ID || node.Ref != interactionRef {
						t.Errorf("the changed_by_run of the %s %s ends at %+v, want the interaction of run %s at %s",
							changed.what, changed.id, node, run.ID, interactionRef)
					}
					if byRun[0].Excerpt == nil || byRun[0].Excerpt.Text != excerpt {
						t.Errorf("the interaction of run %s at the changed_by_run of the %s %s answers the excerpt %+v, want the run's words cut at 240 runes, %q, character for character",
							run.ID, changed.what, changed.id, byRun[0].Excerpt, excerpt)
					}
				}
			}
		}
	}

	// Each node the run changed holds one changed_by_run tuple, to the interaction of the run.
	for _, changed := range changedNodes {
		held, err := graphstore.CurrentOut(ctx, database.Pool, changed.kind, changed.id, []string{graphstore.TupleChangedByRun})
		if err != nil {
			t.Fatalf("read the changed_by_run of the %s %s: %v", changed.what, changed.id, err)
		}
		t.Logf("the changed_by_run of the %s %s in the store: %+v", changed.what, changed.id, held)
		if len(held) != 1 || held[0].ToKind != graphstore.KindInteraction || held[0].ToID != run.ID {
			t.Fatalf("the %s %s holds the changed_by_run %+v, want one to the interaction of run %s", changed.what, changed.id, held, run.ID)
		}
	}

	// Alice reads the interaction at its own address, both ways: her words whole as its text, and,
	// out of it, its one authored_by, to her person node, and its frames, the two it appended and
	// then the terminal one, in sequence.
	for _, direction := range []string{"to", "from"} {
		interaction := referencesAt(t, handler, direction, interactionRef, alice)
		t.Logf("alice's ?%s=%s: status=%d body=%s", direction, interactionRef, interaction.Code, interaction.Body.String())
		if interaction.Code != http.StatusOK {
			t.Fatalf("alice's ?%s= of her run's interaction answers %d %s", direction, interaction.Code, interaction.Body.String())
		}
		answer := decodeBody[model.GraphReferences](t, interaction)
		if answer.Node.Kind != graphstore.KindInteraction || answer.Node.Ref != interactionRef {
			t.Errorf("alice's ?%s= of %s answers the node %+v", direction, interactionRef, answer.Node)
		}
		if answer.Node.Text != words {
			t.Errorf("alice's ?%s= of %s answers the text %q, %d runes, want the run's whole words, %d runes, character for character",
				direction, interactionRef, answer.Node.Text, len([]rune(answer.Node.Text)), len(whole))
		}
		if direction == "to" {
			continue
		}
		authored := slices.DeleteFunc(slices.Clone(answer.Edges), func(edge model.GraphEdge) bool { return edge.Kind != graphstore.TupleAuthoredBy })
		if len(authored) != 1 || authored[0].Node.Kind != graphstore.KindPerson || authored[0].Node.ID != alice {
			t.Errorf("alice's run answers the authored_by %+v, want one to her person node", authored)
		}
		if len(answer.Node.Frames) < len(frames) {
			t.Fatalf("alice's run answers the frames %+v, want at least the %d it stored", answer.Node.Frames, len(frames))
		}
		for index, want := range frames {
			got := answer.Node.Frames[index]
			data, err := json.Marshal(got.Data)
			if err != nil {
				t.Fatalf("encode frame %d's data: %v", index+1, err)
			}
			wantData, err := json.Marshal(want.Data)
			if err != nil {
				t.Fatalf("encode the stored frame %d's data: %v", index+1, err)
			}
			if got.Sequence != index+1 || got.Name != want.Name || !jsonEqual(t, data, string(wantData)) {
				t.Errorf("frame value %d of the run is %d %s %s, want %d %s %s", index+1, got.Sequence, got.Name, data, index+1, want.Name, wantData)
			}
		}
	}

	// At the far end of an edge a run carries the head of its words and no frame: alice's ?to= of
	// her own person node - the read behind GET /api/v1/references?to=, which a person node, with no
	// dispatch:// address of its own, is read by here - answers the run at the far end of its
	// authored_by with its excerpt, and none of its frames, which the run's own address alone answers.
	into, err := refs.Edges(ctx, database.Pool, model.Actor{Kind: "user", ID: alice},
		refs.Query{Direction: "in", Kind: graphstore.KindPerson, ID: alice})
	if err != nil {
		t.Fatalf("read alice's ?to= of her person node: %v", err)
	}
	t.Logf("alice's ?to= of her person node answers %d edge(s)", len(into))
	byAuthor := slices.DeleteFunc(into, func(edge model.GraphEdge) bool {
		return edge.Kind != graphstore.TupleAuthoredBy || edge.Node.Kind != graphstore.KindInteraction || edge.Node.ID != run.ID
	})
	if len(byAuthor) != 1 {
		t.Fatalf("alice's ?to= of her person node answers %+v, want the run %s once at the far end of its authored_by", byAuthor, run.ID)
	}
	if byAuthor[0].Excerpt == nil || byAuthor[0].Excerpt.Text != excerpt {
		t.Errorf("the run at the far end of alice's authored_by answers the excerpt %+v, want her words cut at 240 runes, %q", byAuthor[0].Excerpt, excerpt)
	}
	if len(byAuthor[0].Node.Frames) != 0 {
		t.Errorf("the run at the far end of alice's authored_by answers %d frame(s) %+v, want none: a run's frames are its own address's alone",
			len(byAuthor[0].Node.Frames), byAuthor[0].Node.Frames)
	}

	// Each tuple points away from the node the run changed, so the issue's count moves alike for
	// both; alice's person node's is one more for her, the run's authored_by, and unmoved for bob.
	moved := func(login string) int { return after[login].issueCount - before[login].issueCount }
	if moved(alice) != moved(bob) {
		t.Errorf("the issue's backlink count moved by %d for alice and by %d for bob, want the same", moved(alice), moved(bob))
	}
	if after[alice].personCount != before[alice].personCount+1 || after[bob].personCount != before[bob].personCount ||
		after[alice].personCount != after[bob].personCount+1 {
		t.Errorf("alice's person node counts %d then %d for her and %d then %d for bob, want one more for her alone, the authored_by of run %s",
			before[alice].personCount, after[alice].personCount, before[bob].personCount, after[bob].personCount, run.ID)
	}
	for _, direction := range []struct {
		name          string
		before, after []string
	}{{"to", before[bob].to, after[bob].to}, {"from", before[bob].from, after[bob].from}} {
		if slices.ContainsFunc(direction.after, func(end string) bool { return strings.Contains(end, graphstore.KindInteraction) }) {
			t.Errorf("bob's ?%s= of the issue answers %v, which names an interaction", direction.name, direction.after)
		}
	}
	for _, body := range after[bob].bodies {
		if strings.Contains(body, excerpt) || strings.Contains(body, run.ID) {
			t.Errorf("bob read the run in %s", body)
		}
	}

	// She then removes, under the same run, an untouched document an earlier run of hers made:
	// that run, recorded after every read above, makes a document on the issue under its own
	// header, whose node then holds that run's changed_by_run. The removal is a write made for
	// her run, which the run's window, moved an hour ahead, still admits after its terminal frame
	// (withinGoingRun reads now() < ends_at, whatever frames the run holds). A document made again
	// under the same title with no run header takes the removed one's node, and neither
	// document's references hold a changed_by_run, to either run: the removal retracted the
	// earlier run's and appended none of its own, and the remake holds none.
	earlier, err := server.recordGoingRequest(ctx, model.Actor{Kind: "user", ID: alice}, "", "Make a notes document on the issue.")
	if err != nil {
		t.Fatalf("record alice's earlier run: %v", err)
	}
	const notes = "run-notes.md"
	made := requestForRun(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]any{"name": notes}, alice, earlier.ID)
	if made.Code != http.StatusCreated {
		t.Fatalf("make the document under the earlier run %s's header: status=%d body=%s", earlier.ID, made.Code, made.Body.String())
	}
	removed := decodeBody[uploadedArtifact](t, made).Artifact
	documents := runDocumentRig{handler: handler, database: database, issue: issue, run: earlier.ID}
	if got, want := documents.outOfDocument(t, "before the removal", removed.ID), []string{graphstore.KindInteraction + "/" + earlier.ID}; !slices.Equal(got, want) {
		t.Fatalf("before the removal alice's references of the document %s (node artifact/%s) hold the changed_by_run %v, want %v, to the earlier run %s",
			removed.ID, removed.RefKey, got, want, earlier.ID)
	}
	if state, held := documents.storedNode(t, "before the removal", removed.RefKey); state != graphstore.StateLive || len(held) != 1 ||
		held[0].ToKind != graphstore.KindInteraction || held[0].ToID != earlier.ID {
		t.Fatalf("before the removal the node artifact/%s is %s and states the changed_by_run %+v, want it live and stating one, to the earlier run %s",
			removed.RefKey, state, held, earlier.ID)
	}
	removal := requestForRun(t, handler, http.MethodDelete, "/api/v1/artifacts/"+removed.ID, nil, alice, run.ID)
	if removal.Code != http.StatusNoContent {
		t.Fatalf("remove the document %s under run %s's header: status=%d body=%s, want 204", removed.ID, run.ID, removal.Code, removal.Body.String())
	}
	again := emptyDocument(t, handler, issue, notes)
	if again.RefKey != removed.RefKey || again.ID == removed.ID {
		t.Fatalf("the document made again is %s at %s, want a new document at the removed one's %s, so it is the same node",
			again.ID, again.RefKey, removed.RefKey)
	}
	for _, document := range []struct{ what, id string }{{"removed document", removed.ID}, {"document made again under its title", again.ID}} {
		if held := documents.outOfDocument(t, "after the removal and the remake", document.id); len(held) != 0 {
			t.Errorf("alice's references of the %s %s (node artifact/%s) hold the changed_by_run %v, want none, to run %s or to the earlier run %s",
				document.what, document.id, removed.RefKey, held, run.ID, earlier.ID)
		}
	}
	if state, held := documents.storedNode(t, "after the removal and the remake", again.RefKey); state != graphstore.StateLive || len(held) != 0 {
		t.Errorf("the node artifact/%s of the document made again is %s and states the changed_by_run %+v, want it live and stating none, to run %s or to the earlier run %s",
			again.RefKey, state, held, run.ID, earlier.ID)
	}
}

// A write that names a run it may not write for is refused before it runs, as the stream refuses
// a run nobody holds, and stores nothing: another person's run, a run past its window, a run the
// record does not hold and a header that names no uuid each answer 404 GOING_REQUEST_NOT_FOUND,
// the issue holds no comment more, and no tuple points at the run. The refusal claims no
// Idempotency-Key: the same request under the same key, without the header, then writes.
func TestAWriteNamingARunItMayNotWriteForStoresNothing(t *testing.T) {
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	server := directServer(deps)
	ctx := context.Background()
	issue := createTestIssue(t, handler, "RFS", "The issue no refused run writes on")
	record := func(asker string) goingRequest {
		t.Helper()
		run, err := server.recordGoingRequest(ctx, model.Actor{Kind: "user", ID: asker}, "", "What may "+asker+" write for?")
		if err != nil {
			t.Fatalf("record %s's run: %v", asker, err)
		}
		runWindowAhead(t, database, run.ID)
		return run
	}
	bobs := record("bob")
	ended := record("alice")
	if _, err := database.Pool.Exec(ctx, `update going_requests set ends_at = now() - interval '1 second' where id = $1::uuid`, ended.ID); err != nil {
		t.Fatalf("end the run's window: %v", err)
	}
	comments := func() int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from comments where issue_key = $1`, issue).Scan(&count); err != nil {
			t.Fatalf("count the issue's comments: %v", err)
		}
		return count
	}
	held := comments()
	for _, refused := range []struct{ name, run string }{
		{"bob's run", bobs.ID},
		{"alice's run past its window", ended.ID},
		{"a run the record does not hold", uuid.NewString()},
		{"a header that is no uuid", "not-a-run"},
	} {
		response := requestForRun(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
			map[string]any{"body": "A comment written for " + refused.name + "."}, "alice", refused.run)
		t.Logf("a comment written for %s: status=%d body=%s", refused.name, response.Code, response.Body.String())
		if response.Code != http.StatusNotFound || decodeBody[errorBody](t, response).Code != codeGoingRequestNotFound {
			t.Errorf("a comment written for %s answers %d %s, want 404 %s", refused.name, response.Code, response.Body.String(), codeGoingRequestNotFound)
		}
		if count := comments(); count != held {
			t.Errorf("a comment written for %s left the issue with %d comments, want the %d it held", refused.name, count, held)
		}
		if _, err := uuid.Parse(refused.run); err == nil {
			if tuples := tuplesIntoRun(t, database, refused.run); tuples != 0 {
				t.Errorf("a comment written for %s left %d tuples pointing at the run, want none", refused.name, tuples)
			}
		}
	}

	const key = "run-refused-claims-no-key"
	keyed := func(run string) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"body": "A comment under a key."})
		request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+issue+"/comments", bytes.NewReader(data))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Dispatch-User", "alice")
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
		if run != "" {
			request.Header.Set(contracts.GoingRunHeader, run)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if refused := keyed(bobs.ID); refused.Code != http.StatusNotFound {
		t.Fatalf("a keyed comment written for bob's run answers %d %s, want 404", refused.Code, refused.Body.String())
	}
	if written := keyed(""); written.Code != http.StatusCreated {
		t.Fatalf("the same keyed comment written for no run answers %d %s, want 201: the refusal claims no key", written.Code, written.Body.String())
	}
}

// A request whose run withinGoingRun refused reaches the key layer only to be answered from its
// key (answerRefusedRun), and that layer never runs its handler for it, whatever the key holds by
// the time the layer reads it: withinGoingRun's look at the key and the layer's are two
// statements, and the key can stop answering between them. A key that holds nothing - its first
// request released it after a 5xx - is answered the run's refusal and claims no key; a first
// request still running is 409 KEY_IN_FLIGHT; one whose lease lapsed with nothing committed is
// answered the run's refusal and is not taken over; and one that answered is replayed, with the
// run header part of what the repeat is judged by. The rig is the key layer alone, with the
// refusal put on each refused request's context as withinGoingRun puts it there.
func TestTheKeyLayerNeverRunsTheHandlerOfARefusedRun(t *testing.T) {
	running := make(chan struct{})
	release, releaseOnce := heldUntilReleased(t)
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		if run == 1 {
			close(running)
			<-release
		}
		answerRun(run, w, r)
	})
	run := uuid.NewString()
	send := func(key string, refused bool) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/test/a/writes", http.NoBody)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Dispatch-User", "alice")
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
		request.Header.Set(contracts.GoingRunHeader, run)
		if refused {
			request = request.WithContext(context.WithValue(request.Context(), goingRunRefusedKey{}, goingRunRefused{login: "alice", key: key}))
		}
		response := httptest.NewRecorder()
		rig.handler.ServeHTTP(response, request)
		return response
	}
	refusedAs := func(what string, response *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		t.Logf("%s: status=%d body=%s", what, response.Code, response.Body.String())
		if got, _ := refusalOf(t, response); response.Code != status || got != code {
			t.Errorf("%s answers %d %s, want %d %s", what, response.Code, response.Body.String(), status, code)
		}
	}
	attempt := func(key string) int {
		t.Helper()
		var attempt int
		if err := rig.database.Pool.QueryRow(context.Background(),
			`select attempt from request_keys where login = 'alice' and idempotency_key = $1`, key).Scan(&attempt); err != nil {
			t.Fatalf("read the attempt key %s holds: %v", key, err)
		}
		return attempt
	}

	refusedAs("a refused run under a key that holds nothing", send("released", true), http.StatusNotFound, codeGoingRequestNotFound)
	if rows := rig.rows(t); rows != 0 {
		t.Errorf("a refused run under a key that holds nothing left %d request_keys rows, want none: it claims no key", rows)
	}

	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- send("held", false) }()
	<-running
	refusedAs("a refused run under a key whose first request still runs", send("held", true), http.StatusConflict, contracts.IdempotencyKeyInFlight)
	if _, err := rig.database.Pool.Exec(context.Background(), `update request_keys set leased_at = now() - interval '2 minutes'`); err != nil {
		t.Fatalf("lapse the lease: %v", err)
	}
	refusedAs("a refused run under a key whose holder's lease lapsed with nothing committed", send("held", true), http.StatusNotFound, codeGoingRequestNotFound)
	if held := attempt("held"); held != 1 {
		t.Errorf("a refused run took over the key its holder's lapsed lease left: it holds attempt %d, want the holder's 1", held)
	}
	releaseOnce()
	first := awaitResponse(t, answered)
	if first.Code != http.StatusCreated || !strings.Contains(first.Body.String(), `"run":1`) {
		t.Fatalf("the first request under the key: status=%d body=%s, want run 1", first.Code, first.Body.String())
	}
	repeat := send("held", true)
	t.Logf("a refused run under a key whose first request answered: status=%d replayed=%t body=%s", repeat.Code, replayed(repeat), repeat.Body.String())
	if repeat.Code != http.StatusCreated || !replayed(repeat) || repeat.Body.String() != first.Body.String() {
		t.Errorf("a refused run under a key whose first request answered: status=%d replayed=%t body=%s, want the first answer %s replayed",
			repeat.Code, replayed(repeat), repeat.Body.String(), first.Body.String())
	}
	if runs := rig.runs.Load(); runs != 1 {
		t.Errorf("the handler ran %d times, want the first request's once: a refused run never runs it", runs)
	}
}

// A run named in the header in another of the spellings uuid.Parse admits - RFC 4122's URN form,
// the braced form, upper case - is the run its canonical id names, and a write made for it is
// admitted exactly as one naming that id is. withinGoingRun sends the parsed uuid's own spelling
// on, to the lookup and to the context, and never the header's: sent as written, the URN form
// reaches readLiveGoingRun's $1::uuid, which Postgres refuses ("invalid input syntax for type
// uuid", SQLSTATE 22P02), and the write answers 500 where the canonical id answers 201; and a
// spelling put on the context would be the far end of every changed_by_run the write appends,
// which names no interaction node. Each comment written for the run, under each spelling, answers
// 201 and holds the one changed_by_run its write appended, to the run's canonical id, and no
// tuple points at the spelling as the header wrote it.
func TestAWriteNamingARunInAnotherUuidSpellingIsAdmittedAsTheCanonicalID(t *testing.T) {
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	server := directServer(deps)
	ctx := context.Background()
	issue := createTestIssue(t, handler, "RUU", "The issue a run named in every spelling writes on")
	run, err := server.recordGoingRequest(ctx, model.Actor{Kind: "user", ID: "alice"}, "", "Write for me, however my run is spelled.")
	if err != nil {
		t.Fatalf("record alice's run: %v", err)
	}
	runWindowAhead(t, database, run.ID)
	for _, spelling := range []struct{ name, header string }{
		{"the canonical id", run.ID},
		{"the URN form", "urn:uuid:" + run.ID},
		{"the braced form", "{" + run.ID + "}"},
		{"upper case", strings.ToUpper(run.ID)},
	} {
		response := requestForRun(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
			map[string]any{"body": "A comment written for the run named in " + spelling.name + "."}, "alice", spelling.header)
		t.Logf("a comment written for the run named in %s (%s): status=%d body=%s", spelling.name, spelling.header, response.Code, response.Body.String())
		if response.Code != http.StatusCreated {
			t.Errorf("a comment written for the run named in %s (%s) answers %d %s, want 201, as one naming the canonical id is answered",
				spelling.name, spelling.header, response.Code, response.Body.String())
			continue
		}
		comment := decodeBody[writtenRecord](t, response).ID
		if held := changedByRunCount(t, database, graphstore.KindComment, comment, run.ID); held != 1 {
			t.Errorf("the comment written for the run named in %s holds %d changed_by_run tuples to the canonical id %s, want the one its write appended",
				spelling.name, held, run.ID)
		}
		if spelling.header == run.ID {
			continue
		}
		if stray := tuplesIntoRun(t, database, spelling.header); stray != 0 {
			t.Errorf("%d tuples point at the run as %s spells it (%s), want none: a changed_by_run names the canonical id",
				stray, spelling.name, spelling.header)
		}
	}
}

// A version written for a run names as changed by it each block whose words it changed and each
// block it dropped, and not a block it left as it was.
func TestAVersionWrittenForARunNamesTheBlocksItChanged(t *testing.T) {
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	server := directServer(deps)
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "RVB", "A spec a run edits",
		":::requirement{#kept}\nThe kept words stay.\n:::\n\n:::requirement{#edited}\nThe first words.\n:::\n\n:::requirement{#dropped}\nThe dropped words.\n:::\n")
	run, err := server.recordGoingRequest(ctx, model.Actor{Kind: "user", ID: "alice"}, "", "Edit the spec for me.")
	if err != nil {
		t.Fatalf("record alice's run: %v", err)
	}
	runWindowAhead(t, database, run.ID)
	edited := requestForRun(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts/spec/edits", map[string]any{
		"ops": []map[string]any{
			{"op": "replace", "find": "The first words.", "with": "The second words."},
			{"op": "delete", "block": "dropped"},
		},
	}, "alice", run.ID)
	t.Logf("the edit written for the run: status=%d body=%s", edited.Code, edited.Body.String())
	if edited.Code != http.StatusOK && edited.Code != http.StatusCreated {
		t.Fatalf("edit the spec under the run's header: status=%d body=%s", edited.Code, edited.Body.String())
	}
	spec := issue.Key + "/spec"
	for _, block := range []struct {
		id   string
		want int
	}{{"edited", 1}, {"dropped", 1}, {"kept", 0}} {
		if got := changedByRunCount(t, database, refs.BlockNodeKind, spec+"#"+block.id, run.ID); got != block.want {
			t.Errorf("block %s holds %d changed_by_run tuples to the run, want %d", block.id, got, block.want)
		}
	}
	var state string
	if err := database.Pool.QueryRow(ctx, `select state from graph_nodes where kind = $1 and id = $2`,
		refs.BlockNodeKind, spec+"#dropped").Scan(&state); err != nil || state != graphstore.StateGone {
		t.Fatalf("the dropped block's node is %q (%v), want gone: the edit did not drop it", state, err)
	}
}
