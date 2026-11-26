package api

// going_run_withdrawn_test.go holds R3 of dispatch://INFRA-1523 on the command bar's agent runs
// (going_runs): each answer a call of a run gets records, before the agent is given it, the
// comments and issue messages whose words it carries (recordGoingRunPosts), and once one of them is
// deleted every read of the run gives none of what it stored that may quote the post - its stream
// the withdrawal alone, in the place of its ending; its record stopped with
// goingRunWithdrawnReason; its plan with no summary, no arguments and no value now - and nobody
// approves or revises its plan. A run given only posts that stand reads as it did, an id an answer
// names with none of a post's words records nothing, a follow-up handed an earlier run's notes is
// withdrawn with it, and a run still working when the delete lands writes no frame past it. Every
// read row of goingReadTools is a row of goingPostReads, whose call over a fixture records each
// post its answer carries, or of goingPostlessReads, with why its answer carries none.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The words of the fixture's posts, each held by one post alone.
const (
	withdrawnCommentWord = "harbourcometword"
	withdrawnMessageWord = "quaymeteorword"
)

// withdrawnRunRig is an agent rig with one more issue of DEMO, holding a comment and an issue
// message of alice's, each with a word no other post holds.
type withdrawnRunRig struct {
	goingAgentRig
	issue, comment, message string
}

func newWithdrawnRunRig(t *testing.T) withdrawnRunRig {
	t.Helper()
	rig := newGoingAgentRig(t, nil)
	issue := rig.issue(t, "Where the run reads")
	comment := createThreadComment(t, rig.mux, issue, map[string]any{"body": "The " + withdrawnCommentWord + " reached the harbour at dawn."}, "alice")
	message := createIssueMessage(t, rig.mux, issue, map[string]any{"body": "The " + withdrawnMessageWord + " left the quay at noon."}, "alice")
	return withdrawnRunRig{goingAgentRig: rig, issue: issue, comment: comment.ID, message: message.ID}
}

// read makes call 1 of step of run under token, tool with arguments, and answers what the agent
// is given, which must be a 200.
func (rig withdrawnRunRig) read(t *testing.T, run, token string, step int, tool string, arguments map[string]any) string {
	t.Helper()
	answer, err := rig.call(goingRouteCall(run, token, step, 1, tool, arguments))
	if err != nil {
		t.Fatalf("%s %v was refused: %v", tool, arguments, err)
	}
	if answer.Status != http.StatusOK {
		t.Fatalf("%s %v answered %d %s", tool, arguments, answer.Status, answer.Body)
	}
	return string(answer.Body)
}

// recorded is each post going_run_posts holds for run, as "comment <id>" or "message <id>", in
// order.
func (rig withdrawnRunRig) recorded(t *testing.T, run string) []string {
	t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `
		select case when comment_id is not null then 'comment ' || comment_id::text else 'message ' || message_id::text end
		from going_run_posts where run_id = $1::uuid order by 1
	`, run)
	if err != nil {
		t.Fatal(err)
	}
	posts := []string{}
	for rows.Next() {
		var post string
		if err := rows.Scan(&post); err != nil {
			t.Fatal(err)
		}
		posts = append(posts, post)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return posts
}

// withdrawnFrame is a frame of a run's events: its name and its data.
func withdrawnFrame(t *testing.T, name string, data any) agentwire.Frame {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return agentwire.Frame{Name: name, Data: encoded}
}

// step saves step n of run under token, a model step with note and frames, and answers the
// server's refusal, nil for none.
func (rig withdrawnRunRig) step(run, token string, n int, note string, frames ...agentwire.Frame) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := rig.client.Step(ctx, agentwire.StepRequest{Run: run, LeaseToken: token, Step: n, Kind: agentwire.StepModel,
		Body: json.RawMessage(`{}`), Note: note, Frames: frames})
	return err
}

// end ends run's turn done under token.
func (rig withdrawnRunRig) end(t *testing.T, run, token string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rig.client.End(ctx, agentwire.EndRequest{Run: run, LeaseToken: token, State: agentwire.StateDone}); err != nil {
		t.Fatalf("end run %s: %v", run, err)
	}
}

// quoted is a run of alice's that read the post kind id through tool with arguments, whose answer
// held word, and then wrote word in a tool frame, a reply and its step's note: what the rows below
// withdraw is a run that read the post and quoted it. It answers the run and its lease token, the
// run still working.
func (rig withdrawnRunRig) quoted(t *testing.T, words, kind, id, word, tool string, arguments map[string]any) (string, string) {
	t.Helper()
	run, token := rig.working(t, "alice", words)
	if answer := rig.read(t, run, token, 1, tool, arguments); !strings.Contains(answer, word) {
		t.Fatalf("%s %v answered %s, which holds no %q: the run was not given the post", tool, arguments, answer, word)
	}
	if got, want := rig.recorded(t, run), []string{kind + " " + id}; !slices.Equal(got, want) {
		t.Fatalf("the read recorded %v for the run, want %v", got, want)
	}
	quote := "It says the " + word + " came in."
	if err := rig.step(run, token, 2, quote,
		withdrawnFrame(t, "tool", map[string]any{"label": "Read the post", "note": quote}),
		withdrawnFrame(t, "reply", map[string]any{"text": quote})); err != nil {
		t.Fatalf("save the step that quotes the post: %v", err)
	}
	return run, token
}

// waitOnPlan gives run a plan that waits for alice, whose summary, row arguments and value now
// each hold word, and ends its turn, which leaves it waiting.
func (rig withdrawnRunRig) waitOnPlan(t *testing.T, run, token, word string) {
	t.Helper()
	body := "The " + word + " came in."
	rig.exec(t, `insert into going_plans (run_id, plan, state, summary) values ($1::uuid, 1, 'waiting', $2)`, run, "Post that "+body)
	change, err := json.Marshal(map[string]any{"tool": "dispatch_comment", "arguments": map[string]any{"issue": rig.issue, "body": body}})
	if err != nil {
		t.Fatal(err)
	}
	shown, err := json.Marshal(map[string]any{"item": rig.issue, "issue": rig.issue, "fields": []string{"body"}, "now": map[string]any{"body": body}})
	if err != nil {
		t.Fatal(err)
	}
	rig.exec(t, `insert into going_plan_rows (run_id, plan, row, change, state, shown) values ($1::uuid, 1, 1, $2::jsonb, 'waiting', $3::jsonb)`,
		run, string(change), string(shown))
	rig.end(t, run, token)
	if record := rig.record(t, run); record.state != agentwire.StateWaiting {
		t.Fatalf("the run is %s after its turn ended over a waiting plan, want waiting", record.state)
	}
}

// stream is the whole stream of run's events as alice reads it, resuming after last where it is
// set: the run has ended, or is withdrawn, so the stream ends.
func (rig withdrawnRunRig) stream(t *testing.T, run, last string) string {
	t.Helper()
	headers := map[string]string{"X-Dispatch-User": "alice"}
	if last != "" {
		headers["Last-Event-ID"] = last
	}
	response := goingRequestTo(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run+"/events", nil, headers, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("the stream of %s answered %d %s", run, response.Code, response.Body.String())
	}
	return response.Body.String()
}

// withdrawnRunStream is everything the stream of a withdrawn run whose ending holds sequence
// writes: the withdrawal alone, under that id.
func withdrawnRunStream(sequence int64) string {
	return fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", sequence, agentwire.FrameEnd, goingRunWithdrawnFrame)
}

// ending is the sequence of run's last end frame.
func (rig withdrawnRunRig) ending(t *testing.T, run string) int64 {
	t.Helper()
	var ending int64
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select coalesce(max(sequence), 0) from going_run_events where run_id = $1::uuid and name = $2
	`, run, agentwire.FrameEnd).Scan(&ending); err != nil {
		t.Fatal(err)
	}
	return ending
}

// get is what alice reads at target, which must answer 200.
func (rig withdrawnRunRig) get(t *testing.T, target string) string {
	t.Helper()
	response := dispatchRequest(t, rig.mux, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s answered %d %s", target, response.Code, response.Body.String())
	}
	return response.Body.String()
}

// runRead is run as GET /api/v1/going/runs/{id} answers alice.
func (rig withdrawnRunRig) runRead(t *testing.T, run string) goingRun {
	t.Helper()
	var read goingRun
	if err := json.Unmarshal([]byte(rig.get(t, "/api/v1/going/runs/"+run)), &read); err != nil {
		t.Fatal(err)
	}
	return read
}

// listed is run as History lists it to alice.
func (rig withdrawnRunRig) listed(t *testing.T, run string) goingRun {
	t.Helper()
	for _, conversation := range goingHistory(t, rig.mux, "alice", "").Rows {
		for _, listed := range conversation.Runs {
			if listed.ID == run {
				return listed
			}
		}
	}
	t.Fatalf("History does not list run %s", run)
	return goingRun{}
}

// requireRunWithdrawn fails t unless read, a run as a route answers it, reads stopped with the
// withdrawal's reason and keeps words, its person's own.
func requireRunWithdrawn(t *testing.T, what string, read goingRun, words string) {
	t.Helper()
	if read.State != agentwire.StateStopped || read.StopReason == nil || *read.StopReason != goingRunWithdrawnReason || read.Words != words {
		t.Fatalf("%s reads the run %s, %v, %q; want stopped, %q, with its words %q", what, read.State, read.StopReason, read.Words,
			goingRunWithdrawnReason, words)
	}
}

// TestARunThatReadAPostSinceDeletedIsWithdrawn: a run whose read answered a comment's body, or an
// issue message's, quoted it and proposed a plan that waits; then the post is deleted. Its stream
// from the start, and from a frame before its ending, gives the withdrawal alone under the id of
// the ending - the stop of the run, which had not ended - and a resume at that id gives nothing;
// its record and History read it stopped with the withdrawal's reason and keep its words; its plan
// gives no summary, no arguments and no value now, the Inbox lists it no more, and an approval and
// a revision are refused GOING_RUN_WITHDRAWN. What it stored keeps the words.
func TestARunThatReadAPostSinceDeletedIsWithdrawn(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		kind, word, tool string
		arguments        func(rig withdrawnRunRig) map[string]any
		id               func(rig withdrawnRunRig) string
		remove           func(t *testing.T, rig withdrawnRunRig)
	}{
		{
			kind: "comment", word: withdrawnCommentWord, tool: "dispatch_read_comment",
			arguments: func(rig withdrawnRunRig) map[string]any { return map[string]any{"comment": rig.comment} },
			id:        func(rig withdrawnRunRig) string { return rig.comment },
			remove:    func(t *testing.T, rig withdrawnRunRig) { deleteThreadComment(t, rig.mux, rig.comment, "alice") },
		},
		{
			kind: "message", word: withdrawnMessageWord, tool: "dispatch_read",
			arguments: func(rig withdrawnRunRig) map[string]any { return map[string]any{"message": rig.message} },
			id:        func(rig withdrawnRunRig) string { return rig.message },
			remove:    func(t *testing.T, rig withdrawnRunRig) { deleteIssueMessage(t, rig.mux, rig.message, "alice") },
		},
	} {
		t.Run("an issue "+c.kind, func(t *testing.T) {
			t.Parallel()
			rig := newWithdrawnRunRig(t)
			const words = "What does the post say?"
			run, token := rig.quoted(t, words, c.kind, c.id(rig), c.word, c.tool, c.arguments(rig))
			rig.waitOnPlan(t, run, token, c.word)
			if plan := rig.get(t, "/api/v1/going/runs/"+run+"/plan"); !strings.Contains(plan, c.word) {
				t.Fatalf("before the delete the plan reads %s, without the run's words", plan)
			}
			if plans := rig.get(t, "/api/v1/me/going-plans"); !strings.Contains(plans, run) || !strings.Contains(plans, c.word) {
				t.Fatalf("before the delete the Inbox lists %s, without the run's plan", plans)
			}
			c.remove(t, rig)

			stream := rig.stream(t, run, "")
			ending := rig.ending(t, run)
			if stream != withdrawnRunStream(ending) {
				t.Fatalf("after the delete the stream wrote\n%q\nwant the withdrawal alone\n%q", stream, withdrawnRunStream(ending))
			}
			if resumed := rig.stream(t, run, "1"); resumed != withdrawnRunStream(ending) {
				t.Fatalf("resuming after frame 1 wrote\n%q\nwant the withdrawal alone\n%q", resumed, withdrawnRunStream(ending))
			}
			if past := rig.stream(t, run, fmt.Sprint(ending)); past != "" {
				t.Fatalf("resuming at the ending wrote %q, want nothing: that reader holds the run's one ending", past)
			}
			requireRunWithdrawn(t, "GET /api/v1/going/runs/{id}", rig.runRead(t, run), words)
			requireRunWithdrawn(t, "History", rig.listed(t, run), words)
			if record := rig.record(t, run); record.state != agentwire.StateStopped || record.reason != goingRunWithdrawnReason {
				t.Fatalf("the run's row is %s %q, want it stopped with the withdrawal's reason, so it stores nothing more", record.state, record.reason)
			}

			body := rig.get(t, "/api/v1/going/runs/"+run+"/plan")
			var plan goingPlanAnswer
			if err := json.Unmarshal([]byte(body), &plan); err != nil {
				t.Fatal(err)
			}
			if !plan.Withdrawn || plan.Summary != "" || len(plan.Rows) != 1 || string(plan.Rows[0].Arguments) != "{}" ||
				string(plan.Rows[0].Now) != "null" || strings.Contains(body, c.word) {
				t.Fatalf("after the delete the plan reads %s, want it withdrawn with no summary, no arguments and no value now", body)
			}
			if plans := rig.get(t, "/api/v1/me/going-plans"); strings.Contains(plans, run) || strings.Contains(plans, c.word) {
				t.Fatalf("after the delete the Inbox lists %s, which holds the withdrawn run", plans)
			}
			for verb, answer := range map[string]any{"approve": map[string]any{"plan": 1}, "revise": map[string]any{"plan": 1, "words": "Say it again."}} {
				response := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run+"/plan/"+verb, answer, "alice")
				if refusal := decodeBody[refusalAnswer](t, response); response.Code != http.StatusConflict || refusal.Code != codeGoingRunWithdrawn {
					t.Fatalf("the %s of the withdrawn run's plan answered %d %s, want 409 %s", verb, response.Code, refusal.Code, codeGoingRunWithdrawn)
				}
			}
			if stored := rig.count(t, `select count(*) from going_run_events where run_id = $1::uuid and data::text like '%' || $2 || '%'`, run, c.word); stored == 0 {
				t.Fatalf("the run's stored frames hold no %q: what Dispatch wrote keeps its words, and the reads mask them", c.word)
			}
		})
	}
}

// TestARunGivenOnlyStandingPostsIsUnchanged, the control: a run given a comment that stands
// streams its frames exactly as before, and reads as it ended, when another comment on the same
// issue, one the run was never given, is deleted.
func TestARunGivenOnlyStandingPostsIsUnchanged(t *testing.T) {
	t.Parallel()
	rig := newWithdrawnRunRig(t)
	other := createThreadComment(t, rig.mux, rig.issue, map[string]any{"body": "The lanternotherword is lit elsewhere."}, "alice")
	const words = "What does the comment say?"
	run, token := rig.quoted(t, words, "comment", rig.comment, withdrawnCommentWord, "dispatch_read_comment", map[string]any{"comment": rig.comment})
	rig.end(t, run, token)
	before := rig.stream(t, run, "")
	if !strings.Contains(before, withdrawnCommentWord) {
		t.Fatalf("the stream wrote %q, without the run's words: the control decides nothing", before)
	}
	deleteThreadComment(t, rig.mux, other.ID, "alice")
	if after := rig.stream(t, run, ""); after != before {
		t.Fatalf("after the delete of a comment the run never read, the stream wrote\n%q\nwant it unchanged\n%q", after, before)
	}
	if read := rig.runRead(t, run); read.State != agentwire.StateDone || read.StopReason != nil || read.Words != words {
		t.Fatalf("after the delete of a comment the run never read, it reads %s %v %q, want done as it ended", read.State, read.StopReason, read.Words)
	}
}

// TestAnIDWithNoneOfAPostsWordsRecordsNothing: an answer that names a comment as the one a reply
// answers and a message in a list of references, with none of their words, records neither, and
// their deletes leave the run as it was; the reply whose own words it gives is recorded.
func TestAnIDWithNoneOfAPostsWordsRecordsNothing(t *testing.T) {
	t.Parallel()
	rig := newWithdrawnRunRig(t)
	const replyWords = "A reply with words of its own."
	reply := createThreadComment(t, rig.mux, rig.issue, map[string]any{"body": replyWords, "reply_to": rig.comment}, "bob")
	rig.hooks.set(t, http.MethodGet, "/api/v1/comments/"+reply.ID, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
		WriteJSON(w, http.StatusOK, map[string]any{
			"id": reply.ID, "body": replyWords, "reply_to": rig.comment, "answers": rig.comment,
			"mentions": []map[string]any{{"kind": "message", "id": rig.message, "href": "/issues/" + rig.issue + "/messages/" + rig.message}},
		})
	})
	run, token := rig.working(t, "alice", "Who replied?")
	rig.read(t, run, token, 1, "dispatch_read_comment", map[string]any{"comment": reply.ID})
	if got, want := rig.recorded(t, run), []string{"comment " + reply.ID}; !slices.Equal(got, want) {
		t.Fatalf("the read recorded %v, want the reply alone: an id with none of a post's words is no post given", got)
	}
	rig.end(t, run, token)
	deleteThreadComment(t, rig.mux, rig.comment, "alice")
	deleteIssueMessage(t, rig.mux, rig.message, "alice")
	if read := rig.runRead(t, run); read.State != agentwire.StateDone || read.StopReason != nil {
		t.Fatalf("after the deletes of posts the run named by id alone, it reads %s %v, want done", read.State, read.StopReason)
	}
}

// followUp records a queued run of alice's asked in words that follows up earlier, in its
// conversation, and answers its id.
func (rig withdrawnRunRig) followUp(t *testing.T, earlier, words string) string {
	t.Helper()
	id := uuid.NewString()
	rig.exec(t, `
		insert into going_runs (id, author_login, words, conversation_id, continues, level, mode, state, shape, created_at)
		select $1::uuid, author_login, $2, coalesce(conversation_id, id), id, 'standard', 'ask', 'queued', $4, clock_timestamp()
		from going_runs where id = $3::uuid
	`, id, words, earlier, goingRunShape)
	rig.exec(t, `insert into going_run_grants (run_id, kind, login) values ($1::uuid, 'header', 'alice')`, id)
	return id
}

// TestAFollowUpGivenAnEarlierRunsNotesIsWithdrawnWithIt: a follow-up's claim hands it the notes of
// the run before it, which quote a comment that run read, and records that comment as the
// follow-up's; the comment's delete withdraws both, the follow-up stopped as it works. A later
// follow-up's claim hands on no turn of a withdrawn run, and records none of its posts, so it
// stands.
func TestAFollowUpGivenAnEarlierRunsNotesIsWithdrawnWithIt(t *testing.T) {
	t.Parallel()
	rig := newWithdrawnRunRig(t)
	const earlierWords, followWords, lastWords = "What does the comment say?", "And what then?", "Start over."
	earlier, token := rig.quoted(t, earlierWords, "comment", rig.comment, withdrawnCommentWord, "dispatch_read_comment", map[string]any{"comment": rig.comment})
	rig.end(t, earlier, token)

	follow := rig.followUp(t, earlier, followWords)
	claimed := rig.claim(t, "instance-of-the-follow-up")
	if claimed == nil || claimed.ID != follow {
		t.Fatalf("the claim took %+v, want the follow-up %s", claimed, follow)
	}
	if len(claimed.Turns) != 1 || claimed.Turns[0].Run != earlier || !strings.Contains(strings.Join(claimed.Turns[0].Notes, " "), withdrawnCommentWord) {
		t.Fatalf("the follow-up was handed %+v, want the earlier run's turn with its notes, which quote the comment", claimed.Turns)
	}
	if got, want := rig.recorded(t, follow), []string{"comment " + rig.comment}; !slices.Equal(got, want) {
		t.Fatalf("the follow-up's claim recorded %v, want the earlier run's post %v", got, want)
	}

	deleteThreadComment(t, rig.mux, rig.comment, "alice")
	requireRunWithdrawn(t, "the follow-up", rig.runRead(t, follow), followWords)
	requireRunWithdrawn(t, "the earlier run", rig.runRead(t, earlier), earlierWords)

	last := rig.followUp(t, follow, lastWords)
	claimed = rig.claim(t, "instance-of-the-last-follow-up")
	if claimed == nil || claimed.ID != last {
		t.Fatalf("the claim took %+v, want the last follow-up %s", claimed, last)
	}
	if len(claimed.Turns) != 0 {
		t.Fatalf("the last follow-up was handed %+v, want no turn of a withdrawn run", claimed.Turns)
	}
	if got := rig.recorded(t, last); len(got) != 0 {
		t.Fatalf("the last follow-up's claim recorded %v, want none: it was handed no turn", got)
	}
	if read := rig.runRead(t, last); read.State != "working" || read.StopReason != nil {
		t.Fatalf("the last follow-up reads %s %v, want working: it read no deleted post", read.State, read.StopReason)
	}
}

// TestARunStillWorkingAtTheDeleteSlipsNoFramePastIt: a reader has a working run's stream open and
// was written the run's first frame before the delete, which is a delivery already sent; the
// run's agent then saves a step that quotes the comment again. The reader is written the
// withdrawal alone after the delete, under the id of the run's one ending, the stop of the run,
// and its stream ends; the run stores nothing more; and a reader that comes later is written the
// withdrawal and nothing else.
func TestARunStillWorkingAtTheDeleteSlipsNoFramePastIt(t *testing.T) {
	t.Parallel()
	rig := newWithdrawnRunRig(t)
	rig.served.deps.Identity = headerIdentity(rig.database)
	server := httptest.NewServer(rig.mux)
	t.Cleanup(server.Close)
	run, token := rig.quoted(t, "What does the comment say?", "comment", rig.comment, withdrawnCommentWord, "dispatch_read_comment",
		map[string]any{"comment": rig.comment})
	status, code, stream := openGoingRunStream(t, server, run, "alice", "")
	if status != http.StatusOK {
		t.Fatalf("the stream answered %d %s", status, code)
	}
	for _, id := range []string{"1", "2"} {
		if frame := stream.next(t, 30*time.Second, "frame "+id); frame.id != id {
			t.Fatalf("before the delete the stream wrote %+v, want frame %s", frame, id)
		}
	}
	deleteThreadComment(t, rig.mux, rig.comment, "alice")
	quote := "It says the " + withdrawnCommentWord + " came in, again."
	// Saved or refused: what holds is that the reader is written none of it.
	_ = rig.step(run, token, 3, quote, withdrawnFrame(t, "reply", map[string]any{"text": quote}))
	frame := stream.next(t, 30*time.Second, "the withdrawal")
	ending := rig.ending(t, run)
	if frame.id != fmt.Sprint(ending) || frame.name != agentwire.FrameEnd || frame.data != string(goingRunWithdrawnFrame) {
		t.Fatalf("after the delete the stream wrote %+v, want the withdrawal under id %d", frame, ending)
	}
	select {
	case extra, more := <-stream.frames:
		if more {
			t.Fatalf("the stream wrote %+v past the withdrawal", extra)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the stream did not end within 30 s of the withdrawal")
	}
	requireAgentCode(t, rig.step(run, token, 4, quote, withdrawnFrame(t, "reply", map[string]any{"text": quote})),
		agentwire.CodeStaleLease, "a step of the withdrawn run")
	if later := rig.stream(t, run, ""); later != withdrawnRunStream(ending) {
		t.Fatalf("a later reader was written\n%q\nwant the withdrawal alone\n%q", later, withdrawnRunStream(ending))
	}
}

// alicesRunStream is the whole stream of run's events as alice reads it from handler: the run has
// ended, or is withdrawn, so the stream ends.
func alicesRunStream(t *testing.T, handler http.Handler, run string) string {
	t.Helper()
	response := goingRequestTo(t, handler, http.MethodGet, "/api/v1/going/runs/"+run+"/events", nil, map[string]string{"X-Dispatch-User": "alice"}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("the stream of %s answered %d %s", run, response.Code, response.Body.String())
	}
	return response.Body.String()
}

// planFramesThenEnd is what the stream of run writes once its own deletes alone withdrew it: each
// plan frame it stored before its ending, read off the store, then the end frame of that
// withdrawal under the ending's id. It fails t where run stored no plan frame, which would leave
// the stream nothing to show of the plan.
func (rig goingPlanRig) planFramesThenEnd(t *testing.T, run string) string {
	t.Helper()
	ctx := context.Background()
	var ending int64
	if err := rig.database.Pool.QueryRow(ctx, `
		select coalesce(max(sequence), 0) from going_run_events where run_id = $1::uuid and name = $2
	`, run, agentwire.FrameEnd).Scan(&ending); err != nil {
		t.Fatal(err)
	}
	rows, err := rig.database.Pool.Query(ctx, `
		select sequence, data from going_run_events where run_id = $1::uuid and name = $2 and sequence < $3 order by sequence
	`, run, goingFramePlan, ending)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var stream strings.Builder
	plans := 0
	for rows.Next() {
		var sequence int64
		var data []byte
		if err := rows.Scan(&sequence, &data); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&stream, "id: %d\nevent: %s\ndata: %s\n\n", sequence, goingFramePlan, data)
		plans++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if plans == 0 || ending == 0 {
		t.Fatalf("run %s stored %d plan frames and its ending at %d, want both", run, plans, ending)
	}
	fmt.Fprintf(&stream, "id: %d\nevent: %s\ndata: %s\n\n", ending, agentwire.FrameEnd, goingRunWithdrawnByRunFrame)
	return stream.String()
}

// readAndDelete scripts words as a run that reads each of the posts c names by ids, then proposes
// the delete of the first, and answers quoting word; it begins the run as alice, approves its plan
// and answers the run once it ended, failing t unless the run recorded every post it read.
func (rig goingPlanRig) readAndDelete(t *testing.T, c goingPostCase, words, word string, ids ...string) string {
	t.Helper()
	reads := make([]agenttest.Call, 0, len(ids))
	for _, id := range ids {
		reads = append(reads, agenttest.Call{Name: "call_tool", Arguments: map[string]any{"tool": c.read, "arguments": map[string]any{c.kind: id}}})
	}
	rig.loop.script.set(words, func(n int) agenttest.Reply {
		switch n {
		case 1:
			return agenttest.Reply{Calls: reads}
		case 2:
			return proposing(c.deleting(ids[0]))
		}
		return answered("I deleted the " + c.kind + " that said the " + word + " was raised at dawn.")
	})
	run := rig.begin(t, goingLoopRun{login: "alice", words: words})
	rig.waiting(t, run)
	if posts := rig.count(t, `select count(*) from going_run_posts where run_id = $1::uuid`, run); posts != len(ids) {
		t.Fatalf("the run recorded %d posts, want the %d it read", posts, len(ids))
	}
	rig.answer(t, run, "approve", map[string]any{"plan": 1})
	rig.ended(t, run, time.Minute)
	return run
}

// requireRunReads fails t unless run, as GET /api/v1/going/runs/{id} and History read it to alice,
// reads stopped with reason and keeps words.
func (rig goingPlanRig) requireRunReads(t *testing.T, run, words, reason string) {
	t.Helper()
	read := decodeBody[goingRun](t, dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run, nil, "alice"))
	for what, got := range map[string]goingRun{"GET /api/v1/going/runs/{id}": read, "History": goingListed(t, rig.mux, run)} {
		if got.State != agentwire.StateStopped || got.StopReason == nil || *got.StopReason != reason || got.Words != words {
			t.Fatalf("%s reads the run %s, %v, %q; want stopped, %q, with its words %q", what, got.State, got.StopReason, got.Words, reason, words)
		}
	}
}

// goingListed is run as History lists it to alice.
func goingListed(t *testing.T, handler http.Handler, run string) goingRun {
	t.Helper()
	for _, conversation := range goingHistory(t, handler, "alice", "").Rows {
		for _, listed := range conversation.Runs {
			if listed.ID == run {
				return listed
			}
		}
	}
	t.Fatalf("History does not list run %s", run)
	return goingRun{}
}

// TestARunWithdrawnOnlyByItsOwnDeleteShowsItsPlan (the architect's ruling of 15:53Z): a run read a
// comment, or an issue message, and deleted it through its approved plan, then answered quoting it,
// so it is withdrawn by its own delete alone. Its record and History read it stopped with
// ANSWER_WITHDRAWN_BY_RUN, keeping its words; its stream writes its plan frames, which carry no
// word, then the end frame with that reason, and none of its other frames; its plan reads as an
// ended run's and not withdrawn - its row, done, with what its write answered, and the deleted
// post's value the delete's mark with no body - with no summary; an approval is refused as an ended
// run's is; and the Inbox does not list it. What it stored keeps its words.
func TestARunWithdrawnOnlyByItsOwnDeleteShowsItsPlan(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	for _, c := range goingPostCases {
		t.Run("an issue "+c.kind, func(t *testing.T) {
			const word = "gangplankword"
			id, _ := c.make(t, rig, "The "+word+" was raised at dawn.")
			words := "read the " + c.kind + " about the gangplank and delete it"
			run := rig.readAndDelete(t, c, words, word, id)
			rig.requireRunReads(t, run, words, goingRunWithdrawnByRunReason)

			if stream, want := alicesRunStream(t, rig.mux, run), rig.planFramesThenEnd(t, run); stream != want || strings.Contains(stream, word) {
				t.Fatalf("the stream wrote\n%q\nwant the run's plan frames, then its withdrawal by its own delete\n%q", stream, want)
			}

			response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/going/runs/"+run+"/plan", nil, "alice")
			plan := decodeBody[goingPlanAnswer](t, response)
			var now goingPostNow
			if response.Code != http.StatusOK || strings.Contains(response.Body.String(), word) || plan.Withdrawn || plan.Summary != "" || len(plan.Rows) != 1 {
				t.Fatalf("the plan answered %d %s, want the plan of an ended run, not withdrawn, with no summary and no word of the post", response.Code, response.Body.String())
			}
			row := plan.Rows[0]
			if row.Tool != c.delete || row.State != "done" || row.Result == nil || row.Result.Status != http.StatusOK || !row.Deleted ||
				!strings.Contains(string(row.Arguments), id) || json.Unmarshal(row.Now, &now) != nil || now.Body != nil || now.DeletedAt == nil {
				t.Fatalf("the plan's row reads %+v (now %s), want the delete done, answered 200, its arguments, the post deleted with no body", row, row.Now)
			}

			approval := dispatchRequest(t, rig.mux, http.MethodPost, "/api/v1/going/runs/"+run+"/plan/approve", map[string]any{"plan": 1}, "alice")
			if refusal := decodeBody[refusalAnswer](t, approval); approval.Code != http.StatusConflict || refusal.Code != codeGoingPlanNotWaiting {
				t.Fatalf("an approval of the plan answered %d %s, want 409 %s, as for any ended run", approval.Code, refusal.Code, codeGoingPlanNotWaiting)
			}
			if plans := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/me/going-plans", nil, "alice").Body.String(); strings.Contains(plans, run) {
				t.Fatalf("the Inbox lists %s, which holds the run", plans)
			}
			if stored := rig.count(t, `select count(*) from going_run_events where run_id = $1::uuid and data::text like '%' || $2 || '%'`, run, word); stored == 0 {
				t.Fatalf("the run's stored frames hold no %q: what Dispatch wrote keeps its words, and the reads mask them", word)
			}
		})
	}
}

// TestARunWithdrawnByAnotherWritersDeleteTooIsWithdrawn (the architect's ruling of 15:53Z): a run
// read two comments and deleted the first through its plan, so it is withdrawn by its own delete
// alone, ANSWER_WITHDRAWN_BY_RUN. Once alice deletes the second, off the bar, a post it read was
// deleted by another writer than the run, and it is withdrawn as before: stopped with
// ANSWER_WITHDRAWN, its stream the withdrawal alone, its plan withdrawn with no summary, no
// arguments and no value now.
func TestARunWithdrawnByAnotherWritersDeleteTooIsWithdrawn(t *testing.T) {
	t.Parallel()
	rig := newGoingPlanRig(t)
	c := goingPostCases[0]
	const word = "gangplankword"
	deleted, _ := c.make(t, rig, "The "+word+" was raised at dawn.")
	kept, _ := c.make(t, rig, "The lanyardword was coiled by the mast.")
	const words = "read the two comments and delete the first"
	run := rig.readAndDelete(t, c, words, word, deleted, kept)
	rig.requireRunReads(t, run, words, goingRunWithdrawnByRunReason)

	c.remove(t, rig, kept)
	rig.requireRunReads(t, run, words, goingRunWithdrawnReason)
	var ending int64
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select coalesce(max(sequence), 0) from going_run_events where run_id = $1::uuid and name = $2
	`, run, agentwire.FrameEnd).Scan(&ending); err != nil {
		t.Fatal(err)
	}
	if stream := alicesRunStream(t, rig.mux, run); stream != withdrawnRunStream(ending) {
		t.Fatalf("the stream wrote\n%q\nwant the withdrawal alone\n%q", stream, withdrawnRunStream(ending))
	}
	if plan := rig.plan(t, run); !plan.Withdrawn || plan.Summary != "" || string(plan.Rows[0].Arguments) != "{}" || string(plan.Rows[0].Now) != "null" {
		t.Fatalf("the plan reads %+v, want it withdrawn with no summary, no arguments and no value now", plan)
	}
}

// The words of the route fixture's other posts, each held by one post alone.
const (
	withdrawnReplyWord      = "gullwingword"
	withdrawnAnswerWord     = "sailanchorword"
	withdrawnDocumentWord   = "specbeaconword"
	withdrawnSuggestionWord = "tidecharterword"
	withdrawnSessionWord    = "lighthouseword"
	withdrawnRepliedWord    = "bellbuoyword"
	withdrawnProjectWord    = "logcharterword"
)

// withdrawnRouteFixture is every post the route rows read, by name: the rig's comment, edited once
// so its history holds its first body and naming DEMO-1, the spec of an issue's document and a
// block of that spec; a reply of bob's to a block of it; the rig's message, edited once; a reply of
// bob's to it; alice's reply to an ask of bob's; a comment on a project's document; an accepted
// suggestion of the issue's document, whose version its body names; and a message to the session
// s1. ids and words are each post's id and word; document is the issue's document, block a block
// of its spec, and accepted the version the suggestion's accept made.
type withdrawnRouteFixture struct {
	rig                                                          withdrawnRunRig
	document, documentIssue, block, projectDoc, projectSlug, ask string
	accepted, projectAccepted                                    int
	ids, words                                                   map[string]string
}

func newWithdrawnRouteFixture(t *testing.T) withdrawnRouteFixture {
	t.Helper()
	rig := newWithdrawnRunRig(t)
	created := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "DEMO", "title": "The document the run reads", "spec": "The quick brown fox", "force": true,
	}))
	blocks := decodeBody[[]model.ArtifactBlock](t, rig.fixture(t, "alice", http.MethodGet, "/api/v1/artifacts/"+created.PrimaryArtifactID+"/blocks", nil))
	if len(blocks) == 0 {
		t.Fatal("the issue's spec has no block")
	}
	fixture := withdrawnRouteFixture{rig: rig, document: created.PrimaryArtifactID, documentIssue: created.Key, block: blocks[0].ID,
		ids: map[string]string{"comment": rig.comment, "message": rig.message}, words: map[string]string{
			"comment": withdrawnCommentWord, "message": withdrawnMessageWord, "reply": withdrawnReplyWord,
			"answer": withdrawnAnswerWord, "document comment": withdrawnDocumentWord, "suggestion": withdrawnSuggestionWord,
			"session message": withdrawnSessionWord, "message reply": withdrawnRepliedWord, "project suggestion": withdrawnProjectWord,
		}}
	rig.fixture(t, "alice", http.MethodPatch, "/api/v1/comments/"+rig.comment, map[string]any{
		"body": "The " + withdrawnCommentWord + " reached the harbour at dusk, as dispatch://DEMO-1, dispatch://" + created.Key +
			"/spec and dispatch://" + created.Key + "/spec#b-" + fixture.block + " say.",
	})
	rig.fixture(t, "alice", http.MethodPatch, "/api/v1/messages/"+rig.message, map[string]any{
		"body": "The " + withdrawnMessageWord + " left the quay at one.",
	})
	fixture.ids["reply"] = createThreadComment(t, rig.mux, rig.issue, map[string]any{
		"body": "Bob saw the " + withdrawnReplyWord + " too.", "reply_to": rig.comment, "reply_to_block": "b1",
	}, "bob").ID
	fixture.ids["message reply"] = createIssueMessage(t, rig.mux, rig.issue, map[string]any{
		"body": "Bob heard the " + withdrawnRepliedWord + " ring.", "in_reply_to": rig.message,
	}, "bob").ID
	fixture.ask = decodeBody[model.Ask](t, rig.fixture(t, "bob", http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": "When does it sail?",
	})).ID
	fixture.ids["answer"] = createThreadComment(t, rig.mux, rig.issue, map[string]any{
		"body": "It sails when the " + withdrawnAnswerWord + " turns.", "ask_id": fixture.ask,
	}, "alice").ID
	uploaded := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, rig.fixture(t, "alice", http.MethodPost, "/api/v1/projects/DEMO/artifacts", map[string]any{
		"name": "harbour-log.md", "content": "# Harbour log\n\nThe tide came in.\n",
	})).Artifact
	fixture.projectDoc, fixture.projectSlug = uploaded.ID, uploaded.Slug
	fixture.ids["document comment"] = decodeBody[model.Comment](t, rig.fixture(t, "alice", http.MethodPost,
		"/api/v1/artifacts/"+fixture.projectDoc+"/comments", map[string]any{"body": "The " + withdrawnDocumentWord + " is in the log."})).ID
	fixture.ids["suggestion"] = acceptSuggestionNamed(t, rig.mux, created.Key, "quick", withdrawnSuggestionWord).ID
	fixture.accepted = versionNamed(t, documentVersionNames(t, rig.mux, fixture.document), withdrawnSuggestionWord)
	fixture.ids["project suggestion"] = decodeBody[model.Comment](t, rig.fixture(t, "alice", http.MethodPost,
		"/api/v1/artifacts/"+fixture.projectDoc+"/comments", map[string]any{
			"body": "Say this instead, " + withdrawnProjectWord + ".", "anchor": map[string]any{"artifact": fixture.projectSlug, "quote": "tide"},
			"suggestion": map[string]string{"replace_with": "flood"},
		})).ID
	rig.fixture(t, "alice", http.MethodPost, "/api/v1/comments/"+fixture.ids["project suggestion"]+"/accept", map[string]any{})
	fixture.projectAccepted = versionNamed(t, documentVersionNames(t, rig.mux, fixture.projectDoc), withdrawnProjectWord)
	fixture.ids["session message"] = createIssueMessage(t, rig.mux, rig.issue, map[string]any{
		"body": "Tell the " + withdrawnSessionWord + " keeper.", "target": "session:" + goingPlanner, "delivery": "btw",
	}, "alice").ID
	return fixture
}

// goingPostCarryingRead is a read row whose answer carries posts: the tool call that reads it over
// the fixture, and the fixture's posts its answer carries, by name.
type goingPostCarryingRead struct {
	tool      string
	arguments func(f withdrawnRouteFixture) map[string]any
	carries   []string
}

// goingPostReads is, by "<METHOD> <pattern>", every read row of goingReadTools whose answer can
// carry a comment's or an issue message's words.
var goingPostReads = map[string]goingPostCarryingRead{
	"GET /api/v1/comments/{id}": {tool: "dispatch_read_comment", carries: []string{"comment"},
		arguments: func(f withdrawnRouteFixture) map[string]any { return map[string]any{"comment": f.ids["comment"]} }},
	"GET /api/v1/comments/{id}/history": {tool: "dispatch_read", carries: []string{"comment"},
		arguments: func(f withdrawnRouteFixture) map[string]any {
			return map[string]any{"ref": "dispatch://" + f.rig.issue + "/comment/" + f.ids["comment"], "history": true}
		}},
	"GET /api/v1/comments/{id}/blocks": {tool: "dispatch_read_comment_blocks", carries: []string{"comment"},
		arguments: func(f withdrawnRouteFixture) map[string]any { return map[string]any{"comment": f.ids["comment"]} }},
	"GET /api/v1/messages/{id}": {tool: "dispatch_read", carries: []string{"message"},
		arguments: func(f withdrawnRouteFixture) map[string]any { return map[string]any{"message": f.ids["message"]} }},
	"GET /api/v1/messages/{id}/history": {tool: "dispatch_read", carries: []string{"message"},
		arguments: func(f withdrawnRouteFixture) map[string]any {
			return map[string]any{"ref": "dispatch://" + f.rig.issue + "/message/" + f.ids["message"], "history": true}
		}},
	"GET /api/v1/messages/{id}/blocks": {tool: "dispatch_read_message_blocks", carries: []string{"message"},
		arguments: func(f withdrawnRouteFixture) map[string]any { return map[string]any{"message": f.ids["message"]} }},
	"GET /api/v1/issues/{key}/messages/{id}": {tool: "dispatch_read", carries: []string{"message reply"},
		arguments: func(f withdrawnRouteFixture) map[string]any {
			return map[string]any{"issue": f.rig.issue, "message": f.ids["message reply"]}
		}},
	"GET /api/v1/issues/{key}/comments": {tool: "dispatch_read", carries: []string{"comment", "reply", "answer"},
		arguments: func(f withdrawnRouteFixture) map[string]any {
			return map[string]any{"ref": "dispatch://" + f.rig.issue + "/comments"}
		}},
	"GET /api/v1/issues/{key}/messages": {tool: "dispatch_list_issue_messages", carries: []string{"message", "message reply"},
		arguments: func(f withdrawnRouteFixture) map[string]any { return map[string]any{"issue": f.rig.issue} }},
	"GET /api/v1/issues/{key}/events": {tool: "dispatch_read", carries: []string{"comment", "message", "reply", "message reply", "answer"},
		arguments: func(f withdrawnRouteFixture) map[string]any {
			return map[string]any{"issue": f.rig.issue, "events": 100}
		}},
	"GET /api/v1/issues/{key}": {tool: "dispatch_read", carries: []string{"suggestion"},
		arguments: func(f withdrawnRouteFixture) map[string]any { return map[string]any{"issue": f.documentIssue} }},
	"GET /api/v1/issues/{key}/artifacts/{slug}": {tool: "dispatch_read", carries: []string{"suggestion"},
		arguments: func(f withdrawnRouteFixture) map[string]any {
			return map[string]any{"issue": f.documentIssue, "artifact": "spec"}
		}},
	"GET /api/v1/artifacts/{id}": {tool: "dispatch_read_document", carries: []string{"suggestion"},
		arguments: func(f withdrawnRouteFixture) map[string]any { return map[string]any{"document": f.document} }},
	"GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number}": {tool: "dispatch_doc_read", carries: []string{"suggestion"},
		arguments: func(f withdrawnRouteFixture) map[string]any {
			return map[string]any{"issue": f.documentIssue, "version": f.accepted}
		}},
	"GET /api/v1/artifacts/{id}/versions/{number}": {tool: "dispatch_doc_read", carries: []string{"suggestion"},
		arguments: func(f withdrawnRouteFixture) map[string]any {
			return map[string]any{"artifact": f.document, "version": f.accepted}
		}},
	"GET /api/v1/search": {tool: "dispatch_search", carries: []string{"comment"},
		arguments: func(withdrawnRouteFixture) map[string]any { return map[string]any{"query": withdrawnCommentWord} }},
	"GET /api/v1/artifacts/{id}/events": {tool: "dispatch_list_document_events", carries: []string{"document comment"},
		arguments: func(f withdrawnRouteFixture) map[string]any { return map[string]any{"document": f.projectDoc} }},
	"GET /api/v1/events/log": {tool: "dispatch_read_event_log", carries: []string{"comment", "message"},
		arguments: func(withdrawnRouteFixture) map[string]any { return map[string]any{"limit": 200} }},
	"GET /api/v1/inbox": {tool: "dispatch_inbox", carries: []string{"answer"},
		arguments: func(withdrawnRouteFixture) map[string]any { return map[string]any{} }},
	"GET /api/v1/me/answers": {tool: "dispatch_list_my_answers", carries: []string{"answer"},
		arguments: func(withdrawnRouteFixture) map[string]any { return map[string]any{} }},
	"GET /api/v1/asks/{id}": {tool: "dispatch_read_ask", carries: []string{"answer"},
		arguments: func(f withdrawnRouteFixture) map[string]any { return map[string]any{"ask": f.ask} }},
	"GET /api/v1/issues/{key}/references": {tool: "dispatch_read_issue_references", carries: []string{"comment"},
		arguments: func(withdrawnRouteFixture) map[string]any { return map[string]any{"issue": "DEMO-1"} }},
	"GET /api/v1/artifacts/{id}/references": {tool: "dispatch_read_document_references", carries: []string{"comment"},
		arguments: func(f withdrawnRouteFixture) map[string]any { return map[string]any{"document": f.document} }},
	"GET /api/v1/projects/{key}/artifacts/{slug}": {tool: "dispatch_read", carries: []string{"project suggestion"},
		arguments: func(f withdrawnRouteFixture) map[string]any {
			return map[string]any{"project": "DEMO", "artifact": f.projectSlug}
		}},
	"GET /api/v1/projects/{key}/artifacts/{slug}/versions/{number}": {tool: "dispatch_doc_read", carries: []string{"project suggestion"},
		arguments: func(f withdrawnRouteFixture) map[string]any {
			return map[string]any{"project": "DEMO", "artifact": f.projectSlug, "version": f.projectAccepted}
		}},
	"GET /api/v1/references": {tool: "dispatch_read", carries: []string{"comment"},
		arguments: func(f withdrawnRouteFixture) map[string]any {
			return map[string]any{"ref": "dispatch://" + f.documentIssue + "/spec#b-" + f.block}
		}},
	"GET /api/v1/reference-targets": {tool: "dispatch_read_reference_targets", carries: []string{"comment"},
		arguments: func(f withdrawnRouteFixture) map[string]any {
			return map[string]any{"ref": []any{"dispatch://" + f.rig.issue + "/comment/" + f.ids["comment"]}}
		}},
	"GET /api/v1/people/{login}/nodes": {tool: "dispatch_list_person_nodes", carries: []string{"comment"},
		arguments: func(withdrawnRouteFixture) map[string]any { return map[string]any{"person": "alice"} }},
	"POST /api/v1/projects/{key}/related": {tool: "dispatch_related", carries: []string{"comment"},
		arguments: func(withdrawnRouteFixture) map[string]any {
			return map[string]any{"project": "DEMO", "title": "The " + withdrawnCommentWord + " reached the harbour"}
		}},
	"GET /api/v1/agents/{session_id}/messages": {tool: "dispatch_list_agent_messages", carries: []string{"session message"},
		arguments: func(withdrawnRouteFixture) map[string]any { return map[string]any{"session": goingPlanner} }},
}

// goingPostlessRead is a read row whose answer carries no comment's or issue message's words: why,
// and, where a call of it over the fixture shows it, that call.
type goingPostlessRead struct {
	reason    string
	tool      string
	arguments func(f withdrawnRouteFixture) map[string]any
}

// The reasons a read row carries no post's words.
const (
	goingNoCallPicks   = "no call of the bar reads it: dispatch_read's command reads it to find an item, and goingPickRead picks no call of it"
	goingDocumentText  = "a document's own text and blocks, never a comment's: the words an accepted suggestion made document text are the document's (INFRA-1523 census row 1.37)"
	goingNoIssuePost   = "messages on no issue, which nobody edits or deletes (NOT_AN_ISSUE_MESSAGE)"
	goingSessionFiles  = "an agent session's files and their versions"
	goingFirstRowAlone = "its tool reads its first row for every call, whose guard and placeholders it fills (goingReadRow), and never this one"
)

// goingPostlessReads is, by "<METHOD> <pattern>", every other read row of goingReadTools, with why
// its answer carries no post's words.
var goingPostlessReads = map[string]goingPostlessRead{
	"GET /api/v1":        {reason: "the route table: methods, paths and descriptions"},
	"GET /api/v1/agents": {reason: "the roster of agent sessions: ids, titles, states and roles"},
	"GET /api/v1/agents/{session_id}/answers":                            {reason: "the calling session's asks' answers, which an ask holds and which are no comment or message"},
	"GET /api/v1/agents/{session_id}/artifacts/{slug}":                   {reason: goingSessionFiles},
	"GET /api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}": {reason: goingSessionFiles},
	"GET /api/v1/artifacts/{id}/asks":                                    {reason: goingNoCallPicks},
	"GET /api/v1/artifacts/{id}/blocks":                                  {reason: goingDocumentText},
	"GET /api/v1/artifacts/{id}/blocks/{block_id}":                       {reason: "one block of a document, and the links to it: a document's own text"},
	"GET /api/v1/artifacts/{id}/changes":                                 {reason: "a document's text between two of its versions"},
	"GET /api/v1/artifacts/{id}/comments":                                {reason: goingNoCallPicks},
	"GET /api/v1/artifacts/{id}/reviews":                                 {reason: "a document's review steps and who approved them"},
	"GET /api/v1/artifacts/{id}/subscribers":                             {reason: "who follows a document"},
	"GET /api/v1/artifacts/{id}/text": {reason: goingDocumentText, tool: "dispatch_doc_read",
		arguments: func(f withdrawnRouteFixture) map[string]any { return map[string]any{"artifact": f.document} }},
	"GET /api/v1/asks/open": {reason: "the open asks that wait on the caller: their questions, never their replies", tool: "dispatch_open_asks",
		arguments: func(withdrawnRouteFixture) map[string]any { return map[string]any{} }},
	"GET /api/v1/asks/{id}/followers":                    {reason: "the sessions that follow an ask"},
	"GET /api/v1/block-links":                            {reason: "the links between two documents' blocks"},
	"GET /api/v1/broadcasts":                             {reason: goingNoIssuePost},
	"GET /api/v1/broadcasts/{id}":                        {reason: goingNoIssuePost},
	"GET /api/v1/ci-reconcile/passes":                    {reason: "the CI reconcile's passes"},
	"GET /api/v1/dashboard/builds":                       {reason: "the dashboard's builds"},
	"GET /api/v1/delivery/measures":                      {reason: "the delivery dashboard's measures"},
	"GET /api/v1/delivery/pipeline":                      {reason: "the delivery pipeline's runs and jobs"},
	"GET /api/v1/delivery/runs/{id}":                     {reason: "one deploy run with its jobs"},
	"GET /api/v1/delivery/timeline":                      {reason: "the delivery timeline's pull requests and runs"},
	"GET /api/v1/github/app-installation/{owner}/{repo}": {reason: "GitHub's answer on the App's installation"},
	"GET /api/v1/handoffs/{id}":                          {reason: "one phase handoff with its body: a handoff is no comment or message"},
	"GET /api/v1/issues":                                 {reason: "issue lists: keys, titles, statuses and labels"},
	"GET /api/v1/projects/{key}/activity":                {reason: "a project's events counted by type per ISO week, which name no post", tool: "read_project_activity", arguments: func(withdrawnRouteFixture) map[string]any { return map[string]any{"project": "DEMO"} }},
	"GET /api/v1/issues/resolve":                         {reason: "the issue an external reference names"},
	"GET /api/v1/issues/{key}/artifacts":                 {reason: "an issue's documents by name, which prints no version name (census C.15)"},
	"GET /api/v1/issues/{key}/artifacts/{slug}/blocks":   {reason: goingDocumentText},
	"GET /api/v1/issues/{key}/artifacts/{slug}/text": {reason: goingDocumentText, tool: "dispatch_doc_read",
		arguments: func(f withdrawnRouteFixture) map[string]any { return map[string]any{"issue": f.documentIssue} }},
	"GET /api/v1/issues/{key}/asks":                       {reason: goingNoCallPicks},
	"GET /api/v1/issues/{key}/collaborators":              {reason: "who works on an issue"},
	"GET /api/v1/issues/{key}/coverage":                   {reason: "how an issue's spec requirements are covered, read from its documents"},
	"GET /api/v1/issues/{key}/decisions":                  {reason: "an issue's decisions, read from its decisions document (census 1.16)"},
	"GET /api/v1/issues/{key}/handoffs":                   {reason: "an issue's phase handoffs, without their bodies"},
	"GET /api/v1/issues/{key}/handoffs/{phase}/latest":    {reason: "one phase handoff with its body: a handoff is no comment or message"},
	"GET /api/v1/issues/{key}/legion":                     {reason: "an issue's Legion state"},
	"GET /api/v1/issues/{key}/results":                    {reason: "an issue's results and their checks"},
	"GET /api/v1/issues/{key}/role-holders":               {reason: "who holds an issue's roles"},
	"GET /api/v1/issues/{key}/steps":                      {reason: "an issue's steps"},
	"GET /api/v1/issues/{key}/subscribers":                {reason: "who follows an issue"},
	"GET /api/v1/issues/{key}/summary":                    {reason: "an issue's title and status for a link's card (census C.15)"},
	"GET /api/v1/limits":                                  {reason: "Dispatch's limits"},
	"GET /api/v1/me/agents/state":                         {reason: "the caller's read mark, pin and Clear per session"},
	"GET /api/v1/me/documents/{id}/read":                  {reason: "the version of a document the caller last read"},
	"GET /api/v1/me/state":                                {reason: "the caller's read, pin and dismiss marks per issue"},
	"GET /api/v1/near-copy-checks":                        {reason: "the near-copy checks of new issues against documents"},
	"GET /api/v1/near-copy-checks/{id}":                   {reason: "one near-copy check of a new issue against documents"},
	"GET /api/v1/priorities":                              {reason: "the priorities and the work under them"},
	"GET /api/v1/priorities/{id}/history":                 {reason: "one priority's history of its own fields"},
	"GET /api/v1/projects":                                {reason: "the projects"},
	"GET /api/v1/projects/{key}/architecture":             {reason: "a project's architecture components"},
	"GET /api/v1/projects/{key}/architecture-source":      {reason: "the repository a project's architecture is read from"},
	"GET /api/v1/projects/{key}/architecture-sources":     {reason: "the repositories a project's architecture is read from"},
	"GET /api/v1/projects/{key}/artifacts":                {reason: "a project's documents by name, as an issue's are listed"},
	"GET /api/v1/projects/{key}/artifacts/{slug}/blocks":  {reason: goingDocumentText},
	"GET /api/v1/projects/{key}/artifacts/{slug}/summary": {reason: goingNoCallPicks},
	"GET /api/v1/projects/{key}/artifacts/{slug}/text": {reason: goingDocumentText, tool: "dispatch_doc_read",
		arguments: func(f withdrawnRouteFixture) map[string]any {
			return map[string]any{"project": "DEMO", "artifact": f.projectSlug}
		}},
	"GET /api/v1/projects/{key}/legion/board":        {reason: "a project's Legion board"},
	"GET /api/v1/projects/{key}/legion/daemon":       {reason: "a project's Legion daemon setting"},
	"GET /api/v1/projects/{key}/plans":               {reason: "a project's finished issues and their plans"},
	"GET /api/v1/projects/{key}/related":             {reason: "issues related by their words, printed by owner label alone (census C.15)"},
	"GET /api/v1/references/questions":               {reason: "which asks a source cites, by id (census C.15)"},
	"GET /api/v1/related-checks":                     {reason: "how many related-item checks ran"},
	"GET /api/v1/schema/blocks":                      {reason: "the typed blocks' schema"},
	"GET /api/v1/search/coverage":                    {reason: "how much of Dispatch search has indexed"},
	"GET /api/v1/search/results":                     {reason: "dispatch_search reads GET /api/v1/search, its first row, for every call (goingReadRow)"},
	"GET /api/v1/settings/architecture-sources":      {reason: "the deployment's architecture sources"},
	"GET /api/v1/settings/delivery":                  {reason: "the delivery timeline's configuration"},
	"GET /api/v1/settings/new-issue":                 {reason: "the deployment's settings for a new issue"},
	"GET /api/v1/settings/project-default-assignees": {reason: "each project's default assignee"},
	"GET /api/v1/settings/repo-projects":             {reason: "which project each repository maps to"},
	"GET /api/v1/users":                              {reason: "everyone who has signed in"},
	"GET /api/v1/whoami":                             {reason: "the signed-in principal"},
	"POST /api/v1/issues/{key}/results/dry-run":      {reason: "a result check's verdict on what the call sends"},
	"POST /api/v1/markdown/round-trip":               {reason: "the markdown the call sends, as Dispatch stores it"},
	"GET /api/v1/me/replies":                         {reason: goingFirstRowAlone},
}

// goingEveryReadRow is every read row of goingReadTools, "<METHOD> <pattern>", each once: the rows
// an entry names, or the routes its spec binds.
func goingEveryReadRow(t *testing.T) []string {
	t.Helper()
	specs := make(map[string]contracts.DispatchTool, len(contracts.DispatchTools))
	for _, spec := range contracts.DispatchTools {
		specs[spec.Name] = spec
	}
	seen := map[string]bool{}
	for tool, entry := range goingReadTools {
		rows := entry.Routes
		if spec, stated := specs[tool]; stated {
			rows, _ = goingToolRows(spec, entry.Routes, nil)
		}
		for _, row := range rows {
			seen[row.Method+" "+row.Pattern] = true
		}
	}
	rows := make([]string, 0, len(seen))
	for row := range seen {
		rows = append(rows, row)
	}
	sort.Strings(rows)
	return rows
}

// TestEveryReadThatCarriesAPostRecordsIt: every read row of goingReadTools is a row of
// goingPostReads or of goingPostlessReads, and of one alone. A call of each row of goingPostReads,
// over a fixture of every kind of post, records as its run's each post of the fixture whose word
// its answer holds, the posts the row names among them; and a call of a row of goingPostlessReads
// that names one holds no post's word.
func TestEveryReadThatCarriesAPostRecordsIt(t *testing.T) {
	t.Parallel()
	rows := goingEveryReadRow(t)
	for _, row := range rows {
		_, carries := goingPostReads[row]
		_, carriesNone := goingPostlessReads[row]
		if carries == carriesNone {
			t.Errorf("read row %s is in goingPostReads %t and goingPostlessReads %t: put it in one, with a call over the fixture or why it carries no post", row, carries, carriesNone)
		}
	}
	for _, listed := range [](func() []string){
		func() []string { return slices.Collect(maps.Keys(goingPostReads)) },
		func() []string { return slices.Collect(maps.Keys(goingPostlessReads)) },
	} {
		for _, row := range listed() {
			if !slices.Contains(rows, row) {
				t.Errorf("%s is listed, and is no read row of goingReadTools", row)
			}
		}
	}
	if t.Failed() {
		return
	}

	fixture := newWithdrawnRouteFixture(t)
	// call makes the call on a run of its own, and answers what the agent is given and what the
	// run recorded.
	call := func(t *testing.T, row, tool string, arguments map[string]any) (string, []string) {
		t.Helper()
		// A tool that reads its item's record before it picks its row (Choose) names the record's
		// row; any other names the row its call reads.
		if goingReadTools[tool].Choose == nil {
			if picked, ok := GoingReadRoute(tool, arguments, fixture.rig.served.deps.ServerURL); ok && picked != row {
				t.Fatalf("%s %v reads %s, not %s", tool, arguments, picked, row)
			}
		}
		run, token := fixture.rig.working(t, "alice", "Read "+row)
		answer, err := fixture.rig.call(goingRouteCall(run, token, 1, 1, tool, arguments))
		if err != nil {
			t.Fatalf("%s %v was refused: %v", tool, arguments, err)
		}
		return string(answer.Body), fixture.rig.recorded(t, run)
	}
	for row, read := range goingPostReads {
		t.Run(row, func(t *testing.T) {
			answer, recorded := call(t, row, read.tool, read.arguments(fixture))
			for _, name := range read.carries {
				if !strings.Contains(answer, fixture.words[name]) {
					t.Errorf("the answer holds no word of the %s: %s", name, answer)
				}
			}
			for name, word := range fixture.words {
				if strings.Contains(answer, word) && !slices.Contains(recorded, goingPostKind(name)+" "+fixture.ids[name]) {
					t.Errorf("the answer holds the %s's word %q, and the run recorded %v without it", name, word, recorded)
				}
			}
		})
	}
	for row, read := range goingPostlessReads {
		if read.tool == "" {
			continue
		}
		t.Run(row, func(t *testing.T) {
			answer, recorded := call(t, row, read.tool, read.arguments(fixture))
			for name, word := range fixture.words {
				if strings.Contains(answer, word) {
					t.Errorf("the answer holds the %s's word %q, and the row is listed as carrying none: %s", name, word, answer)
				}
			}
			// A document's text holds what an accepted suggestion made of it, which is the
			// document's own and no post's.
			if len(recorded) != 0 {
				t.Errorf("the run recorded %v of an answer listed as carrying no post: %s", recorded, answer)
			}
		})
	}
}

// goingPostKind is the kind of the fixture's post named name.
func goingPostKind(name string) string {
	if strings.Contains(name, "message") {
		return "message"
	}
	return "comment"
}
