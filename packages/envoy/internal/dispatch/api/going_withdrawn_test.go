package api

// going_withdrawn_test.go holds R3 of dispatch://INFRA-1523 on the stored frames of an answer run
// (going_events): once a comment or an issue message whose text a run gave its model is deleted,
// every read of that run's stream from before the run's ending gives none of its frames, no text
// and no item among them, and ends on the withdrawal in the place of the run's ending, and a read
// resumed at or past that ending is written nothing (goingWithdrawal). A run given only posts that
// stand streams as it did before. The read of the run's own address, dispatch://interaction/<uuid>,
// answers the frames the stream answers, before the delete and after it (refs' interactionFrames).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// withdrawnGoingStream is everything the stream writes for a withdrawn run whose ending holds
// sequence: the withdrawal alone, under that id.
func withdrawnGoingStream(sequence int) string {
	return fmt.Sprintf("id: %d\nevent: %s\ndata: {\"code\":%q}\n\n",
		sequence, contracts.GoingEventError, contracts.GoingErrorAnswerWithdrawn)
}

// quotingFrames are the frames of an answer that quotes phrase and cites the post it was read
// from, kind and id: a text frame carrying the post's words, an item frame naming the post, and
// the end.
func quotingFrames(kind, id, phrase string) []going.Event {
	return []going.Event{
		going.Text("The note says " + phrase + "."),
		going.Item(going.Hit{Kind: kind, ID: id, Title: "Posts an answer reads", Href: "/items/" + id}),
		going.End(4, 1),
	}
}

// quotingStream is what the stream of a run that stored quotingFrames writes, byte for byte: each
// payload in the key order jsonb stores it in, as TestGoingStreamReplaysStoredFramesAndResumes
// reads it.
func quotingStream(kind, id, phrase string) string {
	return fmt.Sprintf("id: 1\nevent: text\ndata: {\"text\":\"The note says %s.\"}\n\n", phrase) +
		fmt.Sprintf("id: 2\nevent: item\ndata: {\"id\":%q,\"href\":\"/items/%s\",\"kind\":%q,\"title\":\"Posts an answer reads\"}\n\n", id, id, kind) +
		"id: 3\nevent: end\ndata: {\"items\":1,\"words\":4}\n\n"
}

// askQuotingPost asks a question on project whose run searches for phrase, is given the post kind
// id among the hits with its text, and answers quotingFrames; it waits for the run's ending and
// answers the run. It fails the test unless the run was given that post's text: what the rows
// below withdraw is an answer that read the post.
func askQuotingPost(t *testing.T, rig goingRoutesRig, answerer *fakeAnswerer, project, kind, id, phrase string) askedRun {
	t.Helper()
	return askQuotingPostFound(t, rig, answerer, project, phrase, kind, id, phrase)
}

// askQuotingPostFound is askQuotingPost for a run that searches for searches rather than for
// phrase: the post kind id must still be among its hits, with phrase in its text.
func askQuotingPostFound(t *testing.T, rig goingRoutesRig, answerer *fakeAnswerer, project, searches, kind, id, phrase string) askedRun {
	t.Helper()
	answerer.searches = searches
	answerer.frames = quotingFrames(kind, id, phrase)
	response := rig.ask(t, "alice", map[string]any{"words": "what does the note say about " + phrase + "?", "project": project})
	if response.Code != http.StatusCreated {
		t.Fatalf("ask: status=%d body=%s", response.Code, response.Body.String())
	}
	run := decodeBody[askedRun](t, response)
	if stored := rig.awaitTerminal(t, run.ID); len(stored) != 3 {
		t.Fatalf("the run stored %v, want the three frames it answered", stored)
	}
	_, _, bodies := answerer.read()
	if body := bodies[going.Hit{Kind: kind, ID: id}.Key()]; !strings.Contains(body, phrase) {
		t.Fatalf("the run was given %q for %s %s, want that post's text: bodies %v", body, kind, id, bodies)
	}
	return run
}

// readStream reads the whole of one run's stream as alice, resuming after lastEventID when it is
// not empty, and fails the test on anything but 200.
func (rig goingRoutesRig) readStream(t *testing.T, id, lastEventID string) string {
	t.Helper()
	response := rig.stream(t, id, "alice", lastEventID)
	if response.Code != http.StatusOK {
		t.Fatalf("stream: status=%d body=%s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

// Row 1: a run given a comment, then the comment deleted, then the stream read from the start. The
// read gives the withdrawal alone, under the id of the run's ending, and no frame of the run: no
// text carrying the comment's words and no item naming it.
func TestGoingStreamAfterACommentItReadIsDeletedGivesOnlyTheWithdrawal(t *testing.T) {
	t.Parallel()
	answerer := &fakeAnswerer{}
	rig := newGoingRoutesRig(t, answerer)
	issue := createInteractionIssue(t, rig.handler, "GWC", "Where the answer reads", "The issue the comment is on.")
	const phrase = "zarquonwithdrawncomment"
	comment := createThreadComment(t, rig.handler, issue.Key, map[string]any{"body": "The " + phrase + " drifted."}, "alice")
	run := askQuotingPost(t, rig, answerer, "GWC", "comment", comment.ID, phrase)

	if before := rig.readStream(t, run.ID, ""); before != quotingStream("comment", comment.ID, phrase) {
		t.Fatalf("before the delete the stream wrote\n%q\nwant\n%q", before, quotingStream("comment", comment.ID, phrase))
	}
	deleteThreadComment(t, rig.handler, comment.ID, "alice")

	after := rig.readStream(t, run.ID, "")
	if after != withdrawnGoingStream(3) {
		t.Fatalf("after the delete the stream wrote\n%q\nwant the withdrawal alone\n%q", after, withdrawnGoingStream(3))
	}
	if strings.Contains(after, phrase) {
		t.Fatalf("after the delete the stream carries the comment's words: %q", after)
	}
}

// Row 2: the same for an issue message.
func TestGoingStreamAfterAnIssueMessageItReadIsDeletedGivesOnlyTheWithdrawal(t *testing.T) {
	t.Parallel()
	answerer := &fakeAnswerer{}
	rig := newGoingRoutesRig(t, answerer)
	issue := createInteractionIssue(t, rig.handler, "GWM", "Where the answer reads", "The issue the message is on.")
	const phrase = "zarquonwithdrawnmessage"
	message := createIssueMessage(t, rig.handler, issue.Key, map[string]any{"body": "The " + phrase + " drifted."}, "alice")
	run := askQuotingPost(t, rig, answerer, "GWM", "message", message.ID, phrase)

	if before := rig.readStream(t, run.ID, ""); before != quotingStream("message", message.ID, phrase) {
		t.Fatalf("before the delete the stream wrote\n%q\nwant\n%q", before, quotingStream("message", message.ID, phrase))
	}
	deleteIssueMessage(t, rig.handler, message.ID, "alice")

	after := rig.readStream(t, run.ID, "")
	if after != withdrawnGoingStream(3) {
		t.Fatalf("after the delete the stream wrote\n%q\nwant the withdrawal alone\n%q", after, withdrawnGoingStream(3))
	}
	if strings.Contains(after, phrase) {
		t.Fatalf("after the delete the stream carries the message's words: %q", after)
	}
}

// Row 3: a reader that dropped holding a frame of the answer and reconnects with Last-Event-ID
// after the delete is given the withdrawal alone, under the id of the run's ending, and none of
// the frames past the one it holds. A reader that already holds the ending was written the run's
// one ending and is written nothing more, so no reader is written two endings.
func TestGoingStreamResumedAfterTheDeleteGivesOnlyTheWithdrawal(t *testing.T) {
	t.Parallel()
	answerer := &fakeAnswerer{}
	rig := newGoingRoutesRig(t, answerer)
	issue := createInteractionIssue(t, rig.handler, "GWR", "Where the answer reads", "The issue the comment is on.")
	const phrase = "zarquonresumedcomment"
	comment := createThreadComment(t, rig.handler, issue.Key, map[string]any{"body": "The " + phrase + " drifted."}, "alice")
	run := askQuotingPost(t, rig, answerer, "GWR", "comment", comment.ID, phrase)
	deleteThreadComment(t, rig.handler, comment.ID, "alice")

	for _, held := range []string{"1", "2"} {
		if got := rig.readStream(t, run.ID, held); got != withdrawnGoingStream(3) {
			t.Fatalf("resuming after frame %s wrote\n%q\nwant the withdrawal alone\n%q", held, got, withdrawnGoingStream(3))
		}
	}
	if got := rig.readStream(t, run.ID, "3"); got != "" {
		t.Fatalf("resuming after the ending wrote %q, want nothing: that reader holds the run's one ending", got)
	}
}

// Row 4, the control: a run given a comment that stands streams its frames exactly as before,
// byte for byte, when another comment on the same issue, one the run was never given, is
// deleted. What withdraws an answer is the delete of a post it read, and nothing else.
func TestGoingStreamOfARunGivenOnlyStandingPostsIsUnchanged(t *testing.T) {
	t.Parallel()
	answerer := &fakeAnswerer{}
	rig := newGoingRoutesRig(t, answerer)
	issue := createInteractionIssue(t, rig.handler, "GWS", "Where the answer reads", "The issue the comments are on.")
	const phrase, other = "zarquonstandingcomment", "zarquondeletedelsewhere"
	standing := createThreadComment(t, rig.handler, issue.Key, map[string]any{"body": "The " + phrase + " drifted."}, "alice")
	deleted := createThreadComment(t, rig.handler, issue.Key, map[string]any{"body": "The " + other + " drifted."}, "alice")
	run := askQuotingPost(t, rig, answerer, "GWS", "comment", standing.ID, phrase)
	if _, _, bodies := answerer.read(); bodies[going.Hit{Kind: "comment", ID: deleted.ID}.Key()] != "" {
		t.Fatalf("the run was given the comment this row deletes, %v: the control decides nothing", bodies)
	}

	want := quotingStream("comment", standing.ID, phrase)
	if before := rig.readStream(t, run.ID, ""); before != want {
		t.Fatalf("before the other delete the stream wrote\n%q\nwant\n%q", before, want)
	}
	deleteThreadComment(t, rig.handler, deleted.ID, "alice")
	if after := rig.readStream(t, run.ID, ""); after != want {
		t.Fatalf("after the delete of a comment the run never read, the stream wrote\n%q\nwant it unchanged\n%q", after, want)
	}
	if resumed := rig.readStream(t, run.ID, "1"); resumed != strings.SplitAfterN(want, "\n\n", 2)[1] {
		t.Fatalf("resuming after frame 1 wrote %q, want the frames past it as before", resumed)
	}
}

// heldAnswerer is a model that is still writing when the delete commits: it makes the search and
// the body read a real answer makes, emits first, then waits for release before it emits rest,
// and says what storing rest answered on stored.
type heldAnswerer struct {
	searches string
	first    going.Event
	rest     []going.Event
	release  chan struct{}
	stored   chan error
}

func (h *heldAnswerer) Run(ctx context.Context, request going.Request, emit func(going.Event) error) error {
	hits, err := request.Search(ctx, h.searches)
	if err != nil {
		return emit(going.Fault(contracts.GoingErrorAnswerFailed))
	}
	if _, err := request.Bodies(ctx, hits); err != nil {
		return emit(going.Fault(contracts.GoingErrorAnswerFailed))
	}
	if err := emit(h.first); err != nil {
		return err
	}
	select {
	case <-h.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, frame := range h.rest {
		if err := emit(frame); err != nil {
			h.stored <- err
			return err
		}
	}
	h.stored <- nil
	return nil
}

// Row 5: a run still answering when the delete commits slips no frame past it. A reader has the
// stream open and was written the answer's first frame before the delete, which is a delivery
// already sent; the model then writes the comment's words. Whether the run stored them before
// this reader's next poll or the poll ended the run first, the reader is written the withdrawal
// alone after the delete, under the id of the run's one ending, and a reader that comes later is
// written that withdrawal and nothing else.
func TestGoingRunStillAnsweringAtTheDeleteSlipsNoFramePastIt(t *testing.T) {
	t.Parallel()
	const phrase = "zarquonheldcomment"
	answerer := &heldAnswerer{
		searches: phrase,
		first:    going.Text("Before the delete, "),
		release:  make(chan struct{}),
		stored:   make(chan error, 1),
	}
	rig := newGoingRoutesRig(t, answerer)
	ctx := context.Background()
	issue := createInteractionIssue(t, rig.handler, "GWH", "Where the answer reads", "The issue the comment is on.")
	comment := createThreadComment(t, rig.handler, issue.Key, map[string]any{"body": "The " + phrase + " drifted."}, "alice")
	answerer.rest = quotingFrames("comment", comment.ID, phrase)
	response := rig.ask(t, "alice", map[string]any{"words": "what does the note say about " + phrase + "?", "project": "GWH"})
	if response.Code != http.StatusCreated {
		t.Fatalf("ask: status=%d body=%s", response.Code, response.Body.String())
	}
	run := decodeBody[askedRun](t, response)
	release := sync.OnceFunc(func() { close(answerer.release) })
	t.Cleanup(release)

	sink := &goingStreamSink{header: http.Header{}, flushed: make(chan struct{}, 4)}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/going/requests/"+run.ID+"/stream", nil)
	request.Header.Set("X-Dispatch-User", "alice")
	served := make(chan struct{})
	go func() {
		defer close(served)
		rig.handler.ServeHTTP(sink, request)
	}()
	const first = "id: 1\nevent: text\ndata: {\"text\":\"Before the delete, \"}\n\n"
	waitUntil(t, "the open stream writing the answer's first frame", func() bool {
		return sink.text() == first
	})

	deleteThreadComment(t, rig.handler, comment.ID, "alice")
	release()
	if err := testwait.Receive(t, "the rest of the answer, stored once its release came", answerer.stored); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("storing the rest of the answer answered %v, want it stored or refused as an ended run's", err)
	}
	testwait.Receive(t, "the end of the open stream after the delete", served)

	ending, err := readGoingTerminalEvent(ctx, rig.server.deps.Store.Pool, run.ID)
	if err != nil {
		t.Fatalf("read the run's ending: %v", err)
	}
	if got, want := sink.text(), first+withdrawnGoingStream(ending.Sequence); got != want {
		t.Fatalf("the open stream wrote\n%q\nwant the first frame, then the withdrawal alone\n%q", got, want)
	}
	if later := rig.readStream(t, run.ID, ""); later != withdrawnGoingStream(ending.Sequence) {
		t.Fatalf("a reader after the run ended was written\n%q\nwant the withdrawal alone\n%q", later, withdrawnGoingStream(ending.Sequence))
	}
}

// graphFrameOf is one frame as a graph read of its run answers it (model.GraphFrame): its
// sequence, its name, and data, its payload's JSON, decoded as that read's JSON decodes a frame's,
// so two views of one frame compare as decoded values.
func graphFrameOf(t *testing.T, sequence int, name string, data []byte) model.GraphFrame {
	t.Helper()
	frame := model.GraphFrame{Sequence: sequence, Name: name}
	if err := json.Unmarshal(data, &frame.Data); err != nil {
		t.Fatalf("decode the data %s of frame %d %s: %v", data, sequence, name, err)
	}
	return frame
}

// streamedFrames is every frame a run's stream wrote, in the order written, each its id as the
// sequence, its event as the name and its data decoded as graphFrameOf decodes it.
func streamedFrames(t *testing.T, stream string) []model.GraphFrame {
	t.Helper()
	var frames []model.GraphFrame
	for written := range strings.SplitSeq(stream, "\n\n") {
		if written == "" {
			continue
		}
		var sequence int
		var name, data string
		for line := range strings.SplitSeq(written, "\n") {
			field, value, _ := strings.Cut(line, ": ")
			switch field {
			case "id":
				parsed, err := strconv.Atoi(value)
				if err != nil {
					t.Fatalf("the stream wrote the id %q, which is no sequence: %q", value, stream)
				}
				sequence = parsed
			case "event":
				name = value
			case "data":
				data = value
			default:
				t.Fatalf("the stream wrote the line %q, which no frame holds: %q", line, stream)
			}
		}
		frames = append(frames, graphFrameOf(t, sequence, name, []byte(data)))
	}
	return frames
}

// storedFrames is every frame the run stored, as its going_events rows hold them: the base's
// goingRig.frames reads them as a reader of the stream does, and each `<sequence> <name> <data>`
// line it answers is decoded as graphFrameOf decodes a frame.
func (rig goingRoutesRig) storedFrames(t *testing.T, id string) []model.GraphFrame {
	t.Helper()
	lines := goingRig{server: rig.server, database: rig.database}.frames(t, id)
	frames := make([]model.GraphFrame, 0, len(lines))
	for _, line := range lines {
		sequence, rest, _ := strings.Cut(line, " ")
		name, data, _ := strings.Cut(rest, " ")
		parsed, err := strconv.Atoi(sequence)
		if err != nil {
			t.Fatalf("run %s stored the frame %q, which names no sequence", id, line)
		}
		frames = append(frames, graphFrameOf(t, parsed, name, []byte(data)))
	}
	return frames
}

// graphFrames is the frames alice reads at the run's own address, dispatch://interaction/<uuid>,
// through GET /api/v1/references?from=, which answers them as the node's frames (refs.Node).
func (rig goingRoutesRig) graphFrames(t *testing.T, id string) []model.GraphFrame {
	t.Helper()
	address := "dispatch://" + graphstore.KindInteraction + "/" + id
	read := graphEdges(t, rig.handler, url.Values{"from": {address}})
	if read.Node.Ref != address {
		t.Fatalf("alice's ?from=%s answers the node %+v, want her run", address, read.Node)
	}
	return read.Node.Frames
}

// requireTheGraphReadAnswersTheStream reads the run's stream from the start as alice, and then the
// run's own address, and fails unless the address answers exactly the frames the stream wrote: the
// same sequences, names and data, compared as decoded values. A failure names the first frame the
// graph read answers past what the stream answers, or the first the stream wrote that the graph
// read leaves out. It answers the frames the two answer.
func (rig goingRoutesRig) requireTheGraphReadAnswersTheStream(t *testing.T, id, when string) []model.GraphFrame {
	t.Helper()
	streamed := streamedFrames(t, rig.readStream(t, id, ""))
	read := rig.graphFrames(t, id)
	for index, frame := range read {
		if index >= len(streamed) || !reflect.DeepEqual(frame, streamed[index]) {
			t.Fatalf("%s, the graph read of run %s answers frame %d %s %v past what its stream answers: the stream wrote %+v, and the graph read answers %+v",
				when, id, frame.Sequence, frame.Name, frame.Data, streamed, read)
		}
	}
	if len(streamed) > len(read) {
		left := streamed[len(read)]
		t.Fatalf("%s, the stream of run %s wrote frame %d %s %v, which the graph read leaves out: the stream wrote %+v, and the graph read answers %+v",
			when, id, left.Sequence, left.Name, left.Data, streamed, read)
	}
	return streamed
}

// Row 6: the read of the run's own address is a second view of the frames its stream answers,
// and answers what the stream answers. Before the delete of the comment the run read, both answer
// every frame the run stored, alike; after it, both answer the withdrawal alone, under the sequence
// of the run's ending, and none of the frames the run stored, no text carrying the comment's words
// and no item naming it.
func TestTheGraphReadOfAWithdrawnRunAnswersWhatItsStreamAnswers(t *testing.T) {
	t.Parallel()
	answerer := &fakeAnswerer{}
	rig := newGoingRoutesRig(t, answerer)
	issue := createInteractionIssue(t, rig.handler, "GWG", "Where the answer reads", "The issue the comment is on.")
	const phrase = "zarquongraphreadcomment"
	comment := createThreadComment(t, rig.handler, issue.Key, map[string]any{"body": "The " + phrase + " drifted."}, "alice")
	run := askQuotingPost(t, rig, answerer, "GWG", "comment", comment.ID, phrase)

	stored := rig.storedFrames(t, run.ID)
	if len(stored) != 3 {
		t.Fatalf("run %s stored %+v, want the three frames it answered", run.ID, stored)
	}
	if before := rig.requireTheGraphReadAnswersTheStream(t, run.ID, "before the delete"); !reflect.DeepEqual(before, stored) {
		t.Fatalf("before the delete the stream and the graph read answer %+v, want every frame the run stored, %+v", before, stored)
	}
	deleteThreadComment(t, rig.handler, comment.ID, "alice")

	want := streamedFrames(t, withdrawnGoingStream(3))
	if after := rig.requireTheGraphReadAnswersTheStream(t, run.ID, "after the delete"); !reflect.DeepEqual(after, want) {
		t.Fatalf("after the delete the stream and the graph read answer %+v, want the withdrawal alone, %+v", after, want)
	}
}

// Row 7: a run withdrawn before it stored its ending. The read of its own address answers the
// withdrawal under the sequence the stream's ending of the run then takes, the one past its last
// frame, and writes nothing: the run still holds the one frame it stored, and no ending. The stream
// read after it stores that ending and writes the same one frame, and the address answers it as
// before.
func TestTheGraphReadOfARunWithdrawnBeforeItsEndingAnswersWhatItsStreamWillAnswer(t *testing.T) {
	t.Parallel()
	rig := newGoingRoutesRig(t, nil)
	ctx := context.Background()
	issue := createInteractionIssue(t, rig.handler, "GWE", "Where the answer reads", "The issue the comment is on.")
	const phrase = "zarquonunendedcomment"
	body := "The " + phrase + " drifted."
	comment := createThreadComment(t, rig.handler, issue.Key, map[string]any{"body": body}, "alice")
	run, err := rig.server.recordGoingRequest(ctx, goingAsker, "GWE", "what does the note say about "+phrase+"?")
	if err != nil {
		t.Fatalf("record the run: %v", err)
	}
	// What a run does before it stores a frame of the text it read: it records the post it is given
	// (goingBodies), and then stores the frame.
	given := going.Hit{Kind: "comment", ID: comment.ID}
	if err := rig.server.recordGoingPosts(ctx, run.ID, []going.Hit{given}, map[string]string{given.Key(): body}); err != nil {
		t.Fatalf("record the comment as given to run %s: %v", run.ID, err)
	}
	if _, err := appendGoingEvent(ctx, rig.database.Pool, run.ID, going.Text("The note says "+phrase+".")); err != nil {
		t.Fatalf("store the frame of run %s: %v", run.ID, err)
	}
	stored := rig.storedFrames(t, run.ID)
	deleteThreadComment(t, rig.handler, comment.ID, "alice")

	want := streamedFrames(t, withdrawnGoingStream(2))
	if read := rig.graphFrames(t, run.ID); !reflect.DeepEqual(read, want) {
		t.Fatalf("the graph read of run %s, withdrawn with no ending stored, answers %+v, want the withdrawal alone where its ending goes, %+v", run.ID, read, want)
	}
	if held := rig.storedFrames(t, run.ID); !reflect.DeepEqual(held, stored) {
		t.Fatalf("after the graph read run %s holds %+v, want the frame it stored and no ending, %+v: the read writes nothing", run.ID, held, stored)
	}
	if streamed := rig.requireTheGraphReadAnswersTheStream(t, run.ID, "once the stream ended the run"); !reflect.DeepEqual(streamed, want) {
		t.Fatalf("the stream of run %s, withdrawn with no ending stored, and its graph read after it answer %+v, want the withdrawal alone, %+v", run.ID, streamed, want)
	}
}
