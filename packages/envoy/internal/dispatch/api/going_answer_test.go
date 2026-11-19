package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/goingtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testwait"
)

// goingModelBearer is the bearer the chat client presents to the fake model. The client under
// test is the production one, so it signs each call; a static source is the whole credential this
// test needs, and no deployment's own is read.
const goingModelBearer = "going-test-bearer"

type goingModelTokens struct{}

func (goingModelTokens) Token(context.Context) (string, error) { return goingModelBearer, nil }

// newGoingModelAnswerer is the answerer a deployment runs - going.NewAnswerer over
// going.NewHTTPClient - pointed at goingtest's chat-completions model over httptest. The model is
// the one thing faked: the search, the bodies, the filter, the client's route, its sign-in and its
// reading of the stream are production's, and nothing here calls a service or names a real model
// endpoint.
func newGoingModelAnswerer(t *testing.T, replies ...goingtest.Reply) (*going.Answerer, *goingtest.Model) {
	t.Helper()
	model := goingtest.NewModel(t, replies...)
	client, err := going.NewHTTPClient(going.HTTPConfig{
		Place:    "test",
		Endpoint: model.URL + "/openai/v1",
		Model:    "going-test-deployment",
		Tokens:   goingModelTokens{},
	})
	if err != nil {
		t.Fatalf("build the chat client: %v", err)
	}
	answerer, err := going.NewAnswerer(going.AnswererConfig{Model: client})
	if err != nil {
		t.Fatalf("build the answerer: %v", err)
	}
	return answerer, model
}

// liveGoingFrame is one frame of a stream as a reader on a real connection saw it: the event's
// name and the data it carried.
type liveGoingFrame struct {
	name string
	data string
}

// liveGoingStream is one run's stream on a real HTTP server, read frame by frame as the server
// flushes each, so a test reads what had reached a reader by a point it chooses, such as while the
// model is held (goingtest.Reply.HoldAfterFirstFrame). It is a real connection and not
// httptest.NewRecorder because a recorder buffers the whole response: nothing of what reached a
// reader before the run ended survives it.
type liveGoingStream struct {
	reader *bufio.Reader
	// frames is every frame read so far, in order.
	frames []liveGoingFrame
}

// openLiveGoingStream opens run id's stream as login; its body closes with the test.
func openLiveGoingStream(t *testing.T, server *httptest.Server, id, login string) *liveGoingStream {
	t.Helper()
	// The shared wait's bound (testwait.Context), so a stream that never sends the frame a test
	// reads for fails that read rather than hanging the package.
	ctx := testwait.Context(t)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/going/requests/"+id+"/stream", nil)
	if err != nil {
		t.Fatalf("build the stream request: %v", err)
	}
	request.Header.Set("X-Dispatch-User", login)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream: status=%d", response.StatusCode)
	}
	return &liveGoingStream{reader: bufio.NewReader(response.Body)}
}

// next reads the stream's next frame, or answers the read's error once the stream stops.
func (s *liveGoingStream) next() (liveGoingFrame, error) {
	var frame liveGoingFrame
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return liveGoingFrame{}, err
		}
		switch line = strings.TrimSuffix(line, "\n"); {
		case line == "":
			if frame.name != "" {
				s.frames = append(s.frames, frame)
				return frame, nil
			}
			frame = liveGoingFrame{}
		case strings.HasPrefix(line, "event: "):
			frame.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			frame.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// until reads frames up to the first one named name and answers it. A stream that stops first
// fails the test, naming what it was read for.
func (s *liveGoingStream) until(t *testing.T, name, what string) liveGoingFrame {
	t.Helper()
	for {
		frame, err := s.next()
		if err != nil {
			t.Fatalf("the stream stopped after %d frames %v (%v) before %s", len(s.frames), goingFrameNames(s.frames), err, what)
		}
		if frame.name == name {
			return frame
		}
	}
}

// rest reads the stream to its end and answers every frame it carried, those read before
// included. A stream that does not end on its terminal frame fails the test.
func (s *liveGoingStream) rest(t *testing.T) []liveGoingFrame {
	t.Helper()
	for {
		if _, err := s.next(); err != nil {
			if len(s.frames) == 0 || !(going.Event{Name: s.frames[len(s.frames)-1].name}).Terminal() {
				t.Fatalf("the stream stopped after %d frames (%v), want it to end on its terminal frame", len(s.frames), err)
			}
			return s.frames
		}
	}
}

// answerText is everything the text frames carried, which is the answer a reader saw.
func answerText(t *testing.T, frames []liveGoingFrame) string {
	t.Helper()
	var out strings.Builder
	for _, frame := range frames {
		if frame.name != contracts.GoingEventText {
			continue
		}
		var data struct {
			Text string `json:"text"`
		}
		decodeGoingFrameData(t, frame, &data)
		out.WriteString(data.Text)
	}
	return out.String()
}

// decodeGoingFrameData reads one frame's data into value.
func decodeGoingFrameData(t *testing.T, frame liveGoingFrame, value any) {
	t.Helper()
	if err := json.Unmarshal([]byte(frame.data), value); err != nil {
		t.Fatalf("decode the %s frame %s: %v", frame.name, frame.data, err)
	}
}

// firstGoingFrame is the first frame of that name, and fails the test when the stream holds none.
func firstGoingFrame(t *testing.T, frames []liveGoingFrame, name string) liveGoingFrame {
	t.Helper()
	for _, frame := range frames {
		if frame.name == name {
			return frame
		}
	}
	t.Fatalf("the stream holds no %s frame, only %v", name, goingFrameNames(frames))
	return liveGoingFrame{}
}

// goingFrameNames is every frame's name, in order, for a failure to read.
func goingFrameNames(frames []liveGoingFrame) []string {
	names := make([]string, len(frames))
	for index, frame := range frames {
		names[index] = frame.name
	}
	return names
}

// The whole answer, end to end, with the answerer a deployment runs: Dispatch's own search and
// stored text reach the model, and what the model writes reaches a reader as the stream's frames.
// The item the answer links is named as an item frame carrying that item's own href, the link's
// visible text reaches the reader in the text frames while no byte of its target does, and the
// text the model was given is the item's own stored text - goingItemText's, the text search
// ranked.
//
// The reader here is a real connection, because what it proves is a matter of order and not only
// of bytes: the model is held after the first frame of its reply until this test has read the
// answer's first words from the stream, so those words were readable while the model still had the
// rest to write. A recorder would buffer the whole response and say nothing about what reached a
// reader before the run ended.
func TestGoingAnswersFromWhatDispatchHoldsWhileTheModelIsStillWriting(t *testing.T) {
	const project = "GOA"
	// The project's first issue, as seedGoingProject creates it: the reply the fake model is built
	// with has to name the href before the row exists, and the seeded key is checked against it
	// below rather than assumed.
	const issueKey = project + "-1"
	const href = "/issues/" + issueKey
	const linkText = "The pelican migration"
	link := "[" + linkText + "](" + href + ")"
	// rest holds the model after the first frame of its reply until the test has read that frame's
	// words from the stream (release), so no clock decides whether they came before the end.
	rest := make(chan struct{})
	release := sync.OnceFunc(func() { close(rest) })
	answerer, model := newGoingModelAnswerer(t, goingtest.Reply{
		Text: "The pelicans arrive in spring, and the note Dispatch holds says so: " + link + ".",
		// Four runes a frame over that reply is twenty-seven frames, each a stored text frame of
		// the run's own; the model holds after the first of them.
		Chunk:               4,
		HoldAfterFirstFrame: rest,
	})
	rig := newGoingRoutesRig(t, answerer)
	seededKey, issueTitle := seedGoingProject(t, rig.handler, project)
	if seededKey != issueKey {
		t.Fatalf("the seeded issue is %s, want %s: the model's reply links that href", seededKey, issueKey)
	}
	server := httptest.NewServer(rig.handler)
	t.Cleanup(server.Close)
	// Registered after the model, the rig and this server, so it runs before their cleanups: the
	// rig's wait for its background work and the model's server both wait on the held reply, so a
	// row that fails before it releases the model still ends.
	t.Cleanup(release)

	// Every word of the question that is not a stopword is one the seeded issue is indexed under,
	// so search ranks the issue itself - the item with a body and an href the answer can link -
	// and not only the document beside it.
	const words = "what is the pelican migration?"
	response := rig.ask(t, "alice", map[string]any{"words": words, "project": project})
	if response.Code != http.StatusCreated {
		t.Fatalf("ask: status=%d body=%s", response.Code, response.Body.String())
	}
	run := decodeBody[askedRun](t, response)

	// When it arrived, and not only what: the first text frame is readable while the model is
	// still held with the rest of its reply unwritten. A stream that handed its reader the answer
	// only once the reply had ended would send no text here, and this read would wait until the
	// shared wait gave up.
	stream := openLiveGoingStream(t, server, run.ID, "alice")
	stream.until(t, contracts.GoingEventText, "the first text frame, while the model holds the rest of its reply")
	release()
	frames := stream.rest(t)
	ended := frames[len(frames)-1]
	if ended.name != contracts.GoingEventEnd {
		t.Fatalf("the stream ended with %s %s, want %s: %v", ended.name, ended.data, contracts.GoingEventEnd, goingFrameNames(frames))
	}

	// What the answer says, and what it links: the item frame names the issue search found, and
	// the markdown of the link reaches the reader whole.
	item := firstGoingFrame(t, frames, contracts.GoingEventItem)
	var cited struct {
		ID    string `json:"id"`
		Href  string `json:"href"`
		Kind  string `json:"kind"`
		Title string `json:"title"`
	}
	decodeGoingFrameData(t, item, &cited)
	if cited.Kind != "issue" || cited.ID != issueKey || cited.Href != href || cited.Title != issueTitle {
		t.Errorf("the item frame is %+v, want the seeded issue (issue %s %s %q)", cited, issueKey, href, issueTitle)
	}
	// A text frame carries plain text: the link the model wrote reaches the reader as its own
	// visible text, and the link itself is the item frame above. So the answer still names the
	// item it used, and no target of the model's is ever written into the text.
	answer := answerText(t, frames)
	if !strings.Contains(answer, linkText) {
		t.Errorf("the answer reads %q, want the linked item's own text %q in it", answer, linkText)
	}
	if strings.Contains(answer, href) || strings.Contains(answer, "](") {
		t.Errorf("the answer reads %q, want no target and no link syntax: the item frame is the link", answer)
	}
	for _, unwanted := range []string{contracts.GoingEventChanged, contracts.GoingEventError, contracts.GoingEventNone} {
		for _, frame := range frames {
			if frame.name == unwanted {
				t.Errorf("the stream holds a %s frame (%s), want the answer kept as the model wrote it", unwanted, frame.data)
			}
		}
	}

	// What the model was given: one call, carrying the question and the issue's own stored text,
	// which is what goingItemText reads for the hit and what goingBodies hands the answerer.
	requests := model.Requests()
	if len(requests) != 1 {
		t.Fatalf("the model served %d calls, want the one this answer needed", len(requests))
	}
	var asked string
	for _, message := range requests[0].Messages {
		if message.Role == "user" {
			asked = message.Content
		}
	}
	if !strings.Contains(asked, words) || !strings.Contains(asked, "href: "+href) {
		t.Fatalf("the model was asked %q, want the question and the hit's href", asked)
	}
	stored, err := goingItemText(context.Background(),
		rig.database.Pool, []going.Hit{{Kind: "issue", ID: issueKey}}, contracts.GoingHitRuneMax)
	if err != nil {
		t.Fatalf("read the issue's stored text: %v", err)
	}
	text := stored["issue:"+issueKey]
	if strings.TrimSpace(text) == "" {
		t.Fatalf("the seeded issue holds no stored text, so there is nothing to have shown the model")
	}
	if !strings.Contains(asked, text) {
		t.Errorf("the model was given %q, want the issue's own stored text %q within it", asked, text)
	}
}

// One key, one run (INFRA-258): the same POST sent twice under one Idempotency-Key records one
// question and answers that one run's id both times, the repeat replayed from the key rather than
// run again, so a double press or a client's retry never starts a second answer.
//
// A key holds nothing of the answer: this POST's answer is the run row, which is small enough to
// keep, and the frames of the answer are read from a GET of their own, so the key keeps no part of
// what the model wrote and the repeat is not ANSWER_NOT_KEPT.
func TestGoingRequestUnderOneKeyRecordsOneRun(t *testing.T) {
	answerer := &fakeAnswerer{frames: []going.Event{
		going.Text("Once, "), going.Text("and only once."), going.End(4, 0),
	}}
	rig := newGoingRoutesRig(t, answerer)
	question := map[string]any{"words": "how many runs does one key make?"}
	const key = "going-one-key-one-run"

	first := keyedRequest(t, rig.handler, http.MethodPost, "/api/v1/going/requests", question, "alice", key)
	if first.Code != http.StatusCreated || replayed(first) {
		t.Fatalf("the first ask: status=%d replayed=%v body=%s, want a 201 that ran", first.Code, replayed(first), first.Body.String())
	}
	run := decodeBody[askedRun](t, first)
	frames := rig.awaitTerminal(t, run.ID)

	repeat := keyedRequest(t, rig.handler, http.MethodPost, "/api/v1/going/requests", question, "alice", key)
	if repeat.Code != first.Code || repeat.Body.String() != first.Body.String() || !replayed(repeat) {
		t.Fatalf("the repeat: %d %s replayed=%v, want the first answer %d %s replayed",
			repeat.Code, repeat.Body.String(), replayed(repeat), first.Code, first.Body.String())
	}
	if again := decodeBody[askedRun](t, repeat); again.ID != run.ID {
		t.Fatalf("the repeat answered run %s, want the first run %s", again.ID, run.ID)
	}
	var runs int
	if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from going_requests`).Scan(&runs); err != nil {
		t.Fatalf("count the recorded runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("%d runs were recorded, want one: the repeat is answered from the key, never run again", runs)
	}
	if asked, _, _ := answerer.read(); len(asked) != 1 {
		t.Fatalf("the answerer was asked %v, want the one question", asked)
	}

	// The key answers the run and nothing of the answer: the frames are the stream's, which is a
	// GET of its own, and both answers of the POST carry the run row alone.
	for _, body := range []string{first.Body.String(), repeat.Body.String()} {
		if strings.Contains(body, "Once, ") || strings.Contains(body, contracts.GoingEventText) {
			t.Errorf("the POST answered %s, want the run row with no part of the answer in it", body)
		}
	}
	if code := repeat.Header().Get(contracts.IdempotencyKeyHeader); code != key {
		t.Errorf("the repeat echoed %s = %q, want the key it was judged under", contracts.IdempotencyKeyHeader, key)
	}
	if strings.Contains(repeat.Body.String(), contracts.IdempotencyAnswerNotKept) {
		t.Errorf("the repeat answered %s, want the kept answer: this POST's own is a few hundred bytes", repeat.Body.String())
	}
	want := []string{
		`1 text {"text":"Once, "}`,
		`2 text {"text":"and only once."}`,
		`3 end {"items":0,"words":4}`,
	}
	if strings.Join(frames, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the one run holds\n%s\nwant\n%s", strings.Join(frames, "\n"), strings.Join(want, "\n"))
	}
	stream := rig.stream(t, run.ID, "alice", "")
	if stream.Code != http.StatusOK || !strings.Contains(stream.Body.String(), "and only once.") {
		t.Fatalf("the stream of the keyed run: status=%d body=%q, want the answer's own frames", stream.Code, stream.Body.String())
	}
}
