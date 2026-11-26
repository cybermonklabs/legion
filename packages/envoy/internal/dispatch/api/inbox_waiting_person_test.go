package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheAssigneeArmFollowsWhoAnAskWaitsOn holds the assignee arm (the spec's R2 and
// R4, decisions 10 and 11) to what each of its writers appends, every expected tuple written out
// by hand with the event that caused it: an ask's create reaches the person the ask waits on, who
// takes part in it; and patchIssue's assignee, putAskAskedOf, putProjectDefaultAssignee and the
// reopen's restore of the asks a close withdrew each move every open ask whose waiting person
// changed - the new person's delivered_to and participates_in, the retraction of the delivery of
// whoever it stood to, and the retraction of their participation unless something of their own
// holds them in the thread - under the event of the write that moved it, in a number of statements
// that does not grow with the asks moved, and in that write's own transaction. A closed issue
// moves nothing until its reopen.
func TestTheAssigneeArmFollowsWhoAnAskWaitsOn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	trace := &sentStatements{}
	traced := storetest.TracedStore(t, database, trace)
	handler, _, _ := newTestServer(t, testServerOptions{
		store: traced,
		// The caller's identity is resolved on the untraced pool, so a counted request's
		// statements are its write's own and not the sign-in lookup every request makes.
		identity: headerIdentity(database),
	})
	seedPeople(t, database, "carol", "dana")
	session := "session:" + sessionActor()["id"].(string)

	t.Run("an ask's create reaches the person it waits on, who takes part in it", func(t *testing.T) {
		issue := createInteractionIssue(t, handler, "ARMC", "The create", "A spec")
		assignIssue(t, handler, issue.Key, "bob")
		asked := createPersonAsk(t, handler, issue.Key, "alice", "Should bob ship it?")
		opened := writeEventOf(t, database, "ask.opened", asked.ID)
		thread := "ask:" + asked.ID
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("person:alice > %s @%d", thread, opened),
			fmt.Sprintf("person:bob > %s @%d", thread, opened))
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
			fmt.Sprintf("%s > person:bob @%d", thread, opened))
		// The delivery raises no version (P13): the person it waits on keeps no row of the thread
		// from it, as the old trigger gave them none.
		if logins := threadRowLogins(t, database, asked.ID); !slices.Equal(logins, []string{"alice"}) {
			t.Errorf("the ask's rows are %v, want the asker's alone", logins)
		}
	})

	t.Run("a hand-on moves its ask, and withholds the retraction from a person who wrote in the thread", func(t *testing.T) {
		issue := createInteractionIssue(t, handler, "ARMH", "The hand-on", "A spec")
		assignIssue(t, handler, issue.Key, "bob")

		moved := createPersonAsk(t, handler, issue.Key, "alice", "Who takes the rollout?")
		opened := writeEventOf(t, database, "ask.opened", moved.ID)
		handAskOn(t, handler, moved.ID, "carol", "alice")
		handed := writeEventOf(t, database, "ask.asked_of", moved.ID)
		thread := "ask:" + moved.ID
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("person:alice > %s @%d", thread, opened),
			fmt.Sprintf("person:bob > %s @%d", thread, opened),
			fmt.Sprintf("person:bob > %s @%d retracted", thread, handed),
			fmt.Sprintf("person:carol > %s @%d", thread, handed))
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
			fmt.Sprintf("%s > person:bob @%d", thread, opened),
			fmt.Sprintf("%s > person:bob @%d retracted", thread, handed),
			fmt.Sprintf("%s > person:carol @%d", thread, handed))

		// bob wrote in the second ask's thread, so his own comment holds him there: the hand-on
		// takes his delivery and leaves his part in the thread standing.
		held := createPersonAsk(t, handler, issue.Key, "alice", "Who signs off?")
		heldOpened := writeEventOf(t, database, "ask.opened", held.ID)
		comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "I can.", "ask_id": held.ID}, "bob")
		commented := writeEventOf(t, database, "comment.created", comment.ID)
		handAskOn(t, handler, held.ID, "carol", "alice")
		heldHanded := writeEventOf(t, database, "ask.asked_of", held.ID)
		heldThread := "ask:" + held.ID
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, heldThread,
			fmt.Sprintf("person:alice > %s @%d", heldThread, heldOpened),
			fmt.Sprintf("person:bob > %s @%d", heldThread, heldOpened),
			fmt.Sprintf("person:bob > %s @%d", heldThread, commented),
			fmt.Sprintf("person:carol > %s @%d", heldThread, heldHanded))
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, heldThread,
			fmt.Sprintf("%s > person:bob @%d", heldThread, heldOpened),
			fmt.Sprintf("%s > person:bob @%d retracted", heldThread, heldHanded),
			fmt.Sprintf("%s > person:carol @%d", heldThread, heldHanded))
	})

	t.Run("the asker is reached by nothing of their own ask and keeps their part in it", func(t *testing.T) {
		issue := createInteractionIssue(t, handler, "ARMA", "The asker", "A spec")
		assignIssue(t, handler, issue.Key, "bob")
		asked := createPersonAsk(t, handler, issue.Key, "alice", "Is this mine?")
		opened := writeEventOf(t, database, "ask.opened", asked.ID)
		handAskOn(t, handler, asked.ID, "alice", "alice")
		toAsker := writeEventOf(t, database, "ask.asked_of", asked.ID)
		handAskOn(t, handler, asked.ID, "carol", "alice")
		toCarol := writeEventOf(t, database, "ask.asked_of", asked.ID)
		thread := "ask:" + asked.ID
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("person:alice > %s @%d", thread, opened),
			fmt.Sprintf("person:bob > %s @%d", thread, opened),
			fmt.Sprintf("person:bob > %s @%d retracted", thread, toAsker),
			fmt.Sprintf("person:carol > %s @%d", thread, toCarol))
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
			fmt.Sprintf("%s > person:bob @%d", thread, opened),
			fmt.Sprintf("%s > person:bob @%d retracted", thread, toAsker),
			fmt.Sprintf("%s > person:carol @%d", thread, toCarol))
	})

	t.Run("a change of assignee moves the issue's asks handed to nobody, and none of a closed issue's", func(t *testing.T) {
		issue := createInteractionIssue(t, handler, "ARMP", "The reassignment", "A spec")
		assignIssue(t, handler, issue.Key, "bob")
		follows := createIssueAsk(t, handler, issue.Key, "Does it follow the issue?")
		followsOpened := writeEventOf(t, database, "ask.opened", follows)
		handed := createIssueAsk(t, handler, issue.Key, "Is it carol's?")
		handAskOn(t, handler, handed, "carol", "alice")
		handedOn := writeEventOf(t, database, "ask.asked_of", handed)

		assignIssue(t, handler, issue.Key, "dana")
		reassigned := latestEventOf(t, database, "issue.updated")
		thread := "ask:" + follows
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("%s > %s @%d", session, thread, followsOpened),
			fmt.Sprintf("person:bob > %s @%d", thread, followsOpened),
			fmt.Sprintf("person:bob > %s @%d retracted", thread, reassigned),
			fmt.Sprintf("person:dana > %s @%d", thread, reassigned))
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
			fmt.Sprintf("%s > person:bob @%d", thread, followsOpened),
			fmt.Sprintf("%s > person:bob @%d retracted", thread, reassigned),
			fmt.Sprintf("%s > person:dana @%d", thread, reassigned))
		// The ask handed to carol does not follow the issue, so the change wrote nothing of it.
		if under := tuplesUnder(t, database, reassigned, handed); len(under) != 0 {
			t.Errorf("the change of assignee wrote %v of the ask handed to carol, want nothing", under)
		}
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "ask:"+handed,
			fmt.Sprintf("ask:%s > person:bob @%d", handed, writeEventOf(t, database, "ask.opened", handed)),
			fmt.Sprintf("ask:%s > person:bob @%d retracted", handed, handedOn),
			fmt.Sprintf("ask:%s > person:carol @%d", handed, handedOn))

		// A write that closes the issue as it reassigns it moves nothing: its asks stand on a
		// closed issue, which retracts nothing, so bob's delivery is what the create stored.
		closed := createInteractionIssue(t, handler, "ARMX", "The closed issue", "A spec")
		assignIssue(t, handler, closed.Key, "bob")
		left := createIssueAsk(t, handler, closed.Key, "Left as it was?")
		leftOpened := writeEventOf(t, database, "ask.opened", left)
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+closed.Key, map[string]any{
			"status": "done", "assignee": "dana",
		}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("close and reassign %s: status=%d body=%s", closed.Key, response.Code, response.Body.String())
		}
		closing := latestEventOf(t, database, "issue.closed")
		if under := tuplesUnder(t, database, closing, left); len(under) != 0 {
			t.Errorf("the close of the issue wrote %v of its ask, want nothing: a closed issue retracts nothing", under)
		}
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "ask:"+left,
			fmt.Sprintf("ask:%s > person:bob @%d", left, leftOpened))
	})

	t.Run("a move of the project's default moves every ask that falls through to it, in statements that do not grow with them", func(t *testing.T) {
		one := defaultFallThroughProject(t, handler, "ARM1", 1, 0)
		ten := defaultFallThroughProject(t, handler, "ARMT", 6, 4)
		// Two asks of the project that do not fall through to its default: one on an issue
		// somebody holds, and one handed to somebody.
		held := requireCreatedIssue(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": "ARMT", "title": "Held", "spec": "A spec",
		}, "alice")).Key
		assignIssue(t, handler, held, "dana")
		onHeld := createIssueAsk(t, handler, held, "Is it dana's?")
		handed := createIssueAsk(t, handler, ten.issue, "Is it carol's?")
		handAskOn(t, handler, handed, "carol", "alice")

		statements := func(project string) (int, int64) {
			trace.take()
			response := countedWrite(t, handler, http.MethodPut, "/api/v1/projects/"+project+"/default-assignee",
				map[string]any{"assignee": "bob"}, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("move %s's default: status=%d body=%s", project, response.Code, response.Body.String())
			}
			return len(trace.take()), latestEventOf(t, database, "settings.project_default_assignee.updated")
		}
		oneCount, oneEvent := statements("ARM1")
		tenCount, tenEvent := statements("ARMT")
		t.Logf("moving the default of 1 ask took %d statements, of 10 asks %d", oneCount, tenCount)
		if oneCount == 0 || oneCount != tenCount {
			t.Errorf("moving the default of 1 ask took %d statements and of 10 asks %d, want one count that does not grow with the asks moved", oneCount, tenCount)
		}
		for event, asks := range map[int64][]string{oneEvent: one.asks, tenEvent: ten.asks} {
			for _, ask := range asks {
				want := []string{
					fmt.Sprintf("ask:%s > person:alice retracted", ask),
					fmt.Sprintf("ask:%s > person:bob", ask),
					fmt.Sprintf("person:alice > ask:%s retracted", ask),
					fmt.Sprintf("person:bob > ask:%s", ask),
				}
				if under := tuplesUnder(t, database, event, ask); !slices.Equal(under, want) {
					t.Errorf("the move of the default wrote, of ask %s,\n  %s\nwant\n  %s", ask, strings.Join(under, "\n  "), strings.Join(want, "\n  "))
				}
			}
		}
		for _, ask := range []string{onHeld, handed} {
			if under := tuplesUnder(t, database, tenEvent, ask); len(under) != 0 {
				t.Errorf("the move of the default wrote %v of ask %s, which does not fall through to it", under, ask)
			}
		}
	})

	t.Run("a reopen moves each restored ask's delivery to whoever it waits on now", func(t *testing.T) {
		// The close and the change of assignee in one write: the close withdraws the ask, so the
		// change moves nothing, and the reopen's restore then moves it to dana.
		issue := createInteractionIssue(t, handler, "ARMR", "The reopen", "A spec")
		assignIssue(t, handler, issue.Key, "bob")
		ask := createIssueAsk(t, handler, issue.Key, "Who has it after the reopen?")
		opened := writeEventOf(t, database, "ask.opened", ask)
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
			"status": "done", "assignee": "dana",
		}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("close and reassign %s: status=%d body=%s", issue.Key, response.Code, response.Body.String())
		}
		if under := tuplesUnder(t, database, latestEventOf(t, database, "issue.closed"), ask); len(under) != 0 {
			t.Fatalf("the close of the issue wrote %v of its ask, want nothing: a closed issue retracts nothing", under)
		}
		reopenIssue(t, handler, issue.Key)
		restored := writeEventOf(t, database, "ask.opened", ask)
		if restored == opened {
			t.Fatalf("the reopen appended no ask.opened for ask %s", ask)
		}
		thread := "ask:" + ask
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
			fmt.Sprintf("%s > person:bob @%d", thread, opened),
			fmt.Sprintf("%s > person:bob @%d retracted", thread, restored),
			fmt.Sprintf("%s > person:dana @%d", thread, restored))
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("%s > %s @%d", session, thread, opened),
			fmt.Sprintf("person:bob > %s @%d", thread, opened),
			fmt.Sprintf("person:bob > %s @%d retracted", thread, restored),
			fmt.Sprintf("person:dana > %s @%d", thread, restored))

		// A default moved while the issue was closed moves nothing, and the reopen moves the ask
		// of an issue nobody holds to the default as it stands.
		project := defaultFallThroughProject(t, handler, "ARMS", 1, 0)
		fallsThrough := project.asks[0]
		fallsOpened := writeEventOf(t, database, "ask.opened", fallsThrough)
		closeIssue(t, handler, project.issue)
		putDefaultAssignee(t, handler, "ARMS", "carol")
		if under := tuplesUnder(t, database, latestEventOf(t, database, "settings.project_default_assignee.updated"), fallsThrough); len(under) != 0 {
			t.Fatalf("the move of the default wrote %v of the closed issue's ask, want nothing", under)
		}
		reopenIssue(t, handler, project.issue)
		fallsRestored := writeEventOf(t, database, "ask.opened", fallsThrough)
		fallsThread := "ask:" + fallsThrough
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, fallsThread,
			fmt.Sprintf("%s > person:alice @%d", fallsThread, fallsOpened),
			fmt.Sprintf("%s > person:alice @%d retracted", fallsThread, fallsRestored),
			fmt.Sprintf("%s > person:carol @%d", fallsThread, fallsRestored))
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, fallsThread,
			fmt.Sprintf("%s > %s @%d", session, fallsThread, fallsOpened),
			fmt.Sprintf("person:alice > %s @%d", fallsThread, fallsOpened),
			fmt.Sprintf("person:alice > %s @%d retracted", fallsThread, fallsRestored),
			fmt.Sprintf("person:carol > %s @%d", fallsThread, fallsRestored))
	})

	t.Run("a change of assignee whose move fails leaves neither its tuples nor the assignee", func(t *testing.T) {
		issue := createInteractionIssue(t, handler, "ARMF", "The failed change", "A spec")
		assignIssue(t, handler, issue.Key, "bob")
		ask := createIssueAsk(t, handler, issue.Key, "Does the failure leave it?")
		opened := writeEventOf(t, database, "ask.opened", ask)
		updates := issueEventCount(t, database, issue.Key, "issue.updated")
		// The move's one append fails: a trigger refuses every delivered_to from the ask, which in
		// this write only the move appends - the retraction of bob's delivery and dana's - so PATCH
		// fails after its row, its event and the move's lock and read, at the move's append.
		refuseTuplesFrom(t, database, graphstore.KindAsk, ask, graphstore.TupleDeliveredTo)
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"assignee": "dana"}, "alice")
		if response.Code < http.StatusInternalServerError {
			t.Fatalf("the change of assignee whose move failed answered %d %s, want a 5xx", response.Code, response.Body.String())
		}
		var assignee *string
		if err := database.Pool.QueryRow(ctx, `select assignee from issues where key = $1`, issue.Key).Scan(&assignee); err != nil {
			t.Fatalf("read %s's assignee: %v", issue.Key, err)
		}
		if assignee == nil || *assignee != "bob" {
			t.Errorf("after the failed change %s is assigned to %v, want bob still", issue.Key, assignee)
		}
		if after := issueEventCount(t, database, issue.Key, "issue.updated"); after != updates {
			t.Errorf("the failed change left %d issue.updated events of %s, want the %d before it", after, issue.Key, updates)
		}
		thread := "ask:" + ask
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
			fmt.Sprintf("%s > person:bob @%d", thread, opened))
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("%s > %s @%d", session, thread, opened),
			fmt.Sprintf("person:bob > %s @%d", thread, opened))
	})
}

// TestAPersonsOwnMoveOfAnAskDeliversThemNothing holds the assignee arm's moves to R3's rule that a
// person never sees their own event (outbox's recipientFilter.keep): a person who moves an ask
// onto themselves - by a hand-on, a change of its issue's assignee, a move of its project's
// default, or a reopen that restores it while it waits on them - takes part in its thread and
// draws no delivered_to, while another person's move onto them draws both. That participation is
// the arm's and no write of theirs in the thread (inboxwrite.OwnMove): a move away retracts it,
// beside the retraction of a delivered_to that never stood, which records where the arm left
// them, unless a comment of theirs in the thread holds them there; and a move back by somebody
// else then delivers the ask to them.
func TestAPersonsOwnMoveOfAnAskDeliversThemNothing(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	seedPeople(t, database, "carol", "dana")

	// requireMoved fails unless the person tuples event wrote of ask are want, each written as
	// tuplesUnder writes one, with ASK for the ask's own node.
	requireMoved := func(t *testing.T, what string, event int64, ask string, want ...string) {
		t.Helper()
		for index := range want {
			want[index] = strings.ReplaceAll(want[index], "ASK", "ask:"+ask)
		}
		slices.Sort(want)
		if got := tuplesUnder(t, database, event, ask); !slices.Equal(got, want) {
			t.Errorf("%s wrote, of ask %s,\n  %s\nwant\n  %s", what, ask, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	}
	// assigned is an issue of project key held by bob, with an open ask a session asked on it,
	// which waits on bob and reached him.
	assigned := func(t *testing.T, key string) (ask, issue string) {
		t.Helper()
		created := createInteractionIssue(t, handler, key, "Who has it?", "A spec")
		assignIssue(t, handler, created.Key, "bob")
		return createIssueAsk(t, handler, created.Key, "Who takes it?"), created.Key
	}
	writes := []struct {
		name, key string
		// open makes project key with one open ask a session asked, which waits on first and
		// reached them, and answers that ask and the issue it is on.
		open  func(t *testing.T, key string) (ask, issue string)
		first string
		// move moves the ask onto onto as by, and answers the event the move is recorded under.
		move func(t *testing.T, key, ask, issue, onto, by string) int64
	}{{
		name: "a hand-on", key: "OWNH", open: assigned, first: "bob",
		move: func(t *testing.T, _, ask, _, onto, by string) int64 {
			t.Helper()
			handAskOn(t, handler, ask, onto, by)
			return writeEventOf(t, database, "ask.asked_of", ask)
		},
	}, {
		name: "a change of the issue's assignee", key: "OWNA", open: assigned, first: "bob",
		move: func(t *testing.T, _, _, issue, onto, by string) int64 {
			t.Helper()
			if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue,
				map[string]any{"assignee": onto}, by); response.Code != http.StatusOK {
				t.Fatalf("assign %s to %s as %s: status=%d body=%s", issue, onto, by, response.Code, response.Body.String())
			}
			return latestEventOf(t, database, "issue.updated")
		},
	}, {
		name: "a move of the project's default assignee", key: "OWND",
		open: func(t *testing.T, key string) (string, string) {
			t.Helper()
			project := defaultFallThroughProject(t, handler, key, 1, 0)
			return project.asks[0], project.issue
		},
		first: "alice",
		move: func(t *testing.T, key, _, _, onto, by string) int64 {
			t.Helper()
			if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/"+key+"/default-assignee",
				map[string]any{"assignee": onto}, by); response.Code != http.StatusOK {
				t.Fatalf("move %s's default to %s as %s: status=%d body=%s", key, onto, by, response.Code, response.Body.String())
			}
			return latestEventOf(t, database, "settings.project_default_assignee.updated")
		},
	}}
	for _, write := range writes {
		first := "person:" + write.first
		t.Run(write.name+": a move onto oneself takes part and delivers nothing, and a move away retracts it", func(t *testing.T) {
			key := write.key + "S"
			ask, issue := write.open(t, key)
			requireMoved(t, "carol's move onto herself", write.move(t, key, ask, issue, "carol", "carol"), ask,
				"ASK > "+first+" retracted", first+" > ASK retracted", "person:carol > ASK")
			requireMoved(t, "alice's move of it on to dana", write.move(t, key, ask, issue, "dana", "alice"), ask,
				"ASK > person:carol retracted", "ASK > person:dana", "person:carol > ASK retracted", "person:dana > ASK")
		})
		t.Run(write.name+": another person's move onto them delivers it", func(t *testing.T) {
			key := write.key + "O"
			ask, issue := write.open(t, key)
			requireMoved(t, "dana's move of it on to carol", write.move(t, key, ask, issue, "carol", "dana"), ask,
				"ASK > "+first+" retracted", "ASK > person:carol", first+" > ASK retracted", "person:carol > ASK")
		})
		t.Run(write.name+": a comment of their own holds them past the move away, and a move back by another delivers it", func(t *testing.T) {
			key := write.key + "C"
			ask, issue := write.open(t, key)
			requireMoved(t, "carol's move onto herself", write.move(t, key, ask, issue, "carol", "carol"), ask,
				"ASK > "+first+" retracted", first+" > ASK retracted", "person:carol > ASK")
			createThreadComment(t, handler, issue, map[string]any{"body": "I have it.", "ask_id": ask}, "carol")
			requireMoved(t, "alice's move of it on to dana", write.move(t, key, ask, issue, "dana", "alice"), ask,
				"ASK > person:carol retracted", "ASK > person:dana", "person:dana > ASK")
			requireMoved(t, "dana's move of it back to carol", write.move(t, key, ask, issue, "carol", "dana"), ask,
				"ASK > person:carol", "ASK > person:dana retracted", "person:carol > ASK", "person:dana > ASK retracted")
		})
	}

	t.Run("a reopen by the person the restored ask waits on takes part and delivers nothing", func(t *testing.T) {
		ask, issue := assigned(t, "OWNR")
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{
			"status": "done", "assignee": "carol",
		}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("close %s and hand it to carol: status=%d body=%s", issue, response.Code, response.Body.String())
		}
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue,
			map[string]string{"status": "todo"}, "carol"); response.Code != http.StatusOK {
			t.Fatalf("reopen %s as carol: status=%d body=%s", issue, response.Code, response.Body.String())
		}
		requireMoved(t, "carol's reopen", writeEventOf(t, database, "ask.opened", ask), ask,
			"ASK > person:bob retracted", "person:bob > ASK retracted", "person:carol > ASK")
		assignIssue(t, handler, issue, "dana")
		requireMoved(t, "alice's change of the assignee to dana", latestEventOf(t, database, "issue.updated"), ask,
			"ASK > person:carol retracted", "ASK > person:dana", "person:carol > ASK retracted", "person:dana > ASK")
	})
}

// refuseTuplesFrom makes every append of a tuple of kind from node fromKind/fromID fail, for the
// rest of t: a trigger on graph_tuples the test installs and its cleanup drops, which raises on
// the first such row of a statement, so the write that appends it fails at that statement and
// nowhere before it. The node's id is the test's own, so the trigger reaches no other write.
func refuseTuplesFrom(t *testing.T, database *store.Store, fromKind, fromID, kind string) {
	t.Helper()
	refuseTupleTrigger(t, database,
		`new.from_kind = `+quoteLiteral(fromKind)+` and new.from_id = `+quoteLiteral(fromID)+` and new.kind = `+quoteLiteral(kind),
		`raise exception 'the test refuses % from %/%', new.kind, new.from_kind, new.from_id;`)
}

// refuseTupleTrigger installs, for the rest of t, a row trigger before each insert into
// graph_tuples whose new row passes when, the trigger's WHEN condition, running body, the
// statements of its plpgsql function, which raises where it refuses the row and returns new where
// it lets it through; t's cleanup drops the trigger and its function. Both carry one name made of
// a fresh uuid, so no two installs share one.
func refuseTupleTrigger(t *testing.T, database *store.Store, when, body string) {
	t.Helper()
	ctx := context.Background()
	name := "refuse_" + strings.ReplaceAll(uuid.NewString(), "-", "_")
	if _, err := database.Pool.Exec(ctx, `
		create function `+name+`() returns trigger language plpgsql as $$
		begin
			`+body+`
		end $$`); err != nil {
		t.Fatalf("create the refusing function: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		create trigger `+name+` before insert on graph_tuples for each row
		when (`+when+`)
		execute function `+name+`()`); err != nil {
		t.Fatalf("create the refusing trigger: %v", err)
	}
	t.Cleanup(func() {
		if _, err := database.Pool.Exec(context.Background(), `drop trigger `+name+` on graph_tuples; drop function `+name+`()`); err != nil {
			t.Errorf("drop the refusing trigger: %v", err)
		}
	})
}

// TestAMoveOfTheDefaultTakesTurnsWithEveryWriteThatDecidesWhoAnAskWaitsOn races a move of a
// project's default assignee against the writes that read or move who an ask that falls through
// to it waits on, and holds each pair to ending with one standing delivered_to, to the person
// asks.AskedOfPersonExpression names once both committed (the spec's R2: the list follows the
// assignment); and against the writes into such an ask's thread that read who takes part in it -
// a comment, either first, and an answer - each of which holds the project's lock shared before
// that read (inboxwrite.HoldProjectDefault), so the default the move left keeps the part its own
// write gave it and the default it brought receives what is written after the move (R4).
//
// The interleaving is forced rather than hoped for, as
// TestBroadcastsNamingTheirRecipientsInOppositeOrdersBothCommit forces its own: each write runs on
// a server whose store is one connection of its own, so the backend it runs on is known; a
// transaction holds what the first write needs after it has read who the ask waits on and before it
// commits, and the move is then sent and has to queue behind the first write. A build whose move
// took no turn with the first write would answer the move at once, and the test fails there; a move
// that answered on a lock would leave the first write's delivery standing to the person the move
// replaced.
func TestAMoveOfTheDefaultTakesTurnsWithEveryWriteThatDecidesWhoAnAskWaitsOn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	seedPeople(t, database, "carol", "erin")
	// raceWrites sends first on its own server, waits for it to queue behind holder, then sends
	// second on another, waits for that to queue too - behind holder, or behind first, which holds
	// what second must take turns at - lets holder go, and answers both responses. early says what
	// second answering before holder went means. holder goes however raceWrites ends, so a write
	// still queued behind it when a check fails finishes before its server's cleanup waits for it.
	raceWrites := func(t *testing.T, holder pgx.Tx, first, second func(http.Handler) *httptest.ResponseRecorder, early string) (*httptest.ResponseRecorder, *httptest.ResponseRecorder) {
		t.Helper()
		defer func() { _ = holder.Rollback(context.Background()) }()
		firstServer, firstBackend := oneConnectionHandler(t, deps)
		secondServer, secondBackend := oneConnectionHandler(t, deps)
		firstAnswer := make(chan *httptest.ResponseRecorder, 1)
		go func() { firstAnswer <- first(firstServer) }()
		if answer, answered := storetest.WaitForQueuedBehind(t, holder, firstBackend, firstAnswer); answered {
			t.Fatalf("the first write answered %d before it reached the hold: %s", answer.Code, answer.Body.String())
		}
		secondAnswer := make(chan *httptest.ResponseRecorder, 1)
		go func() { secondAnswer <- second(secondServer) }()
		if answer, answered := storetest.WaitForQueuedBehind(t, holder, secondBackend, secondAnswer); answered {
			t.Fatalf("%s answered %d: %s", early, answer.Code, answer.Body.String())
		}
		if err := holder.Rollback(ctx); err != nil {
			t.Fatalf("let the first write go: %v", err)
		}
		answers := [2]*httptest.ResponseRecorder{}
		for index, answered := range []chan *httptest.ResponseRecorder{firstAnswer, secondAnswer} {
			select {
			case answers[index] = <-answered:
			case <-time.After(storeStepWait):
				t.Fatalf("write %d did not answer within %s of the hold going", index+1, storeStepWait)
			}
		}
		return answers[0], answers[1]
	}
	// moveTo is the move of project's default to bob.
	moveTo := func(t *testing.T, project string) func(http.Handler) *httptest.ResponseRecorder {
		return func(server http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, server, http.MethodPut, "/api/v1/projects/"+project+"/default-assignee",
				map[string]any{"assignee": "bob"}, "alice")
		}
	}
	// race is raceWrites with the move of project's default to bob second.
	race := func(t *testing.T, holder pgx.Tx, project string, first func(http.Handler) *httptest.ResponseRecorder) (*httptest.ResponseRecorder, *httptest.ResponseRecorder) {
		t.Helper()
		return raceWrites(t, holder, first, moveTo(t, project),
			"the move of the default, sent while the first write, which read who the ask waits on, had not committed,")
	}
	// fallingThrough opens carol's question on a new document of a new project, whose default is
	// alice, its creator: it waits on alice, who takes part in its thread by the assignee arm.
	fallingThrough := func(t *testing.T, project string) (document, ask string) {
		t.Helper()
		createDefaultAssigneeProject(t, handler, project, "Race of a thread write")
		document = createProjectDocument(t, handler, project, "Race notes", "# Race\n\nA passage.\n").ID
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document+"/asks", map[string]any{
			"question": "Who takes part in the race?",
			"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
		}, "carol")
		if response.Code != http.StatusCreated {
			t.Fatalf("open carol's question: status=%d body=%s", response.Code, response.Body.String())
		}
		ask = decodeBody[model.Ask](t, response).ID
		requireOneStandingDelivery(t, database, ask, "alice")
		return document, ask
	}
	// commentOn is alice's comment in ask's thread on document.
	commentOn := func(t *testing.T, document, ask string) func(http.Handler) *httptest.ResponseRecorder {
		return func(server http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, server, http.MethodPost, "/api/v1/artifacts/"+document+"/comments",
				map[string]any{"body": "Alice writes in the thread of her question.", "ask_id": ask}, "alice")
		}
	}
	// takesPart is whether login's participates_in in ask's thread stands.
	takesPart := func(t *testing.T, login, ask string) bool {
		t.Helper()
		taking, err := graphstore.CurrentOut(ctx, database.Pool, graphstore.KindPerson, login, []string{graphstore.TupleParticipatesIn})
		if err != nil {
			t.Fatalf("read the threads %s takes part in: %v", login, err)
		}
		return slices.ContainsFunc(taking, func(tuple graphstore.Tuple) bool {
			return tuple.ToKind == graphstore.KindAsk && tuple.ToID == ask
		})
	}
	begin := func(t *testing.T) pgx.Tx {
		t.Helper()
		holder, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the holder: %v", err)
		}
		t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
		return holder
	}

	t.Run("a move of the default racing an ask's create that falls through to it", func(t *testing.T) {
		createDefaultAssigneeProject(t, handler, "RACEC", "Race of a create")
		document := createProjectDocument(t, handler, "RACEC", "Race notes", "# Race\n\nA passage.\n")
		// carol's create keeps her own row of the thread last of all (inboxwrite.Record), after it
		// read the default, alice, and delivered the ask to her: the hold stops it there.
		holder := begin(t)
		if _, err := holder.Exec(ctx, `lock table user_threads in share mode`); err != nil {
			t.Fatalf("hold the writes of user_threads: %v", err)
		}
		created, moved := race(t, holder, "RACEC", func(server http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, server, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
				"question": "Who answers the race?",
				"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
			}, "carol")
		})
		if created.Code != http.StatusCreated || moved.Code != http.StatusOK {
			t.Fatalf("the create answered %d (%s) and the move %d (%s), want both written",
				created.Code, created.Body.String(), moved.Code, moved.Body.String())
		}
		ask := decodeBody[model.Ask](t, created).ID
		requireOneStandingDelivery(t, database, ask, "bob")
	})

	t.Run("a hand-on racing a move of the default over the same ask", func(t *testing.T) {
		createDefaultAssigneeProject(t, handler, "RACEH", "Race of a hand-on")
		document := createProjectDocument(t, handler, "RACEH", "Race notes", "# Race\n\nA passage.\n")
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
			"question": "Who answers the hand-on's race?",
			"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
		}, "carol")
		if response.Code != http.StatusCreated {
			t.Fatalf("open carol's question: status=%d body=%s", response.Code, response.Body.String())
		}
		ask := decodeBody[model.Ask](t, response).ID
		requireOneStandingDelivery(t, database, ask, "alice")
		// The hand-on to erin holds the ask's row from before it reads who the ask waited on, and
		// stops at erin's person node, which no write has made yet and the holder makes and does
		// not commit.
		holder := begin(t)
		if err := graphstore.Write(ctx, holder, []graphstore.Node{
			graphstore.PersonNode("erin", model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC(), graphstore.NoEvent),
		}, nil); err != nil {
			t.Fatalf("hold erin's person node: %v", err)
		}
		handed, moved := race(t, holder, "RACEH", func(server http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, server, http.MethodPut, "/api/v1/asks/"+ask+"/asked-of",
				map[string]any{"asked_of": "erin"}, "alice")
		})
		if handed.Code != http.StatusOK || moved.Code != http.StatusOK {
			t.Fatalf("the hand-on answered %d (%s) and the move %d (%s), want both written",
				handed.Code, handed.Body.String(), moved.Code, moved.Body.String())
		}
		requireOneStandingDelivery(t, database, ask, "erin")
	})

	// Alice, the default the ask falls through to, writes in its thread, and her comment holds
	// the project's lock shared from before it read who takes part (describeAsk): held past its
	// participates_in, at the move of the rows of the people it reached, the comment keeps the move
	// of the default to bob waiting, and the move, once it runs, finds her comment's participation
	// and leaves her in the thread. A move that did not wait would end her part with a retraction
	// newer than her comment's participation, which it could not read yet.
	t.Run("a comment of the default's in an ask's thread racing a move of the default", func(t *testing.T) {
		document, ask := fallingThrough(t, "RACEK")
		holder := begin(t)
		if _, err := holder.Exec(ctx, `lock table user_threads in share mode`); err != nil {
			t.Fatalf("hold the writes of user_threads: %v", err)
		}
		commented, moved := race(t, holder, "RACEK", commentOn(t, document, ask))
		if commented.Code != http.StatusCreated || moved.Code != http.StatusOK {
			t.Fatalf("the comment answered %d (%s) and the move %d (%s), want both written",
				commented.Code, commented.Body.String(), moved.Code, moved.Body.String())
		}
		if !takesPart(t, "alice", ask) {
			t.Errorf("alice, who wrote in the ask's thread, no longer takes part in it once the move of the default committed")
		}
		requireOneStandingDelivery(t, database, ask, "bob")
	})

	// The move of the default to bob is held after it took the project's lock, at the asks rows it
	// moves, and alice's comment in the ask's thread then waits for it at that lock, before it
	// reads who takes part: it reaches bob, whom the move made the person the ask waits on, and
	// alice takes part in the thread from her comment on.
	t.Run("a move of the default racing a comment in the thread of an ask that falls through to it", func(t *testing.T) {
		document, ask := fallingThrough(t, "RACEM")
		holder := begin(t)
		holdRow(t, ctx, holder, `select 1 from asks where id = $1::uuid for no key update`, ask)
		moved, commented := raceWrites(t, holder, moveTo(t, "RACEM"), commentOn(t, document, ask),
			"the comment in the ask's thread, sent while the move of the default it falls through to had not committed,")
		if commented.Code != http.StatusCreated || moved.Code != http.StatusOK {
			t.Fatalf("the comment answered %d (%s) and the move %d (%s), want both written",
				commented.Code, commented.Body.String(), moved.Code, moved.Body.String())
		}
		comment := decodeBody[model.Comment](t, commented).ID
		if reached := meInboxDeliveredTo(t, database, graphstore.KindComment, comment); !slices.Contains(reached, graphstore.KindPerson+":bob") {
			t.Errorf("alice's comment reached %v, want bob, the default the ask fell through to once the move committed", reached)
		}
		if !takesPart(t, "alice", ask) {
			t.Errorf("alice, who wrote in the ask's thread, does not take part in it")
		}
		requireOneStandingDelivery(t, database, ask, "bob")
	})

	// Erin's answer to the ask holds the project's lock shared from before it read who takes part
	// in its thread (transitionAskTx): held past its participates_in, the answer keeps the move of
	// the default waiting, and the move then finds the ask answered and moves nothing of it, so the
	// ask stands delivered to alice, the default it waited on, and to carol, its asker, whom the
	// answer reached, and to nobody else. A move that did not wait would give bob the ask and end
	// alice's delivery, and the answer, planned before it, would then state alice's again beside
	// bob's.
	t.Run("an answer racing a move of the default the ask falls through to", func(t *testing.T) {
		_, ask := fallingThrough(t, "RACEA")
		holder := begin(t)
		if _, err := holder.Exec(ctx, `lock table user_threads in share mode`); err != nil {
			t.Fatalf("hold the writes of user_threads: %v", err)
		}
		answered, moved := race(t, holder, "RACEA", func(server http.Handler) *httptest.ResponseRecorder {
			return dispatchRequest(t, server, http.MethodPost, "/api/v1/asks/"+ask+"/answer",
				map[string]any{"selected": []string{"Yes"}, "expected_edited_at": nil}, "erin")
		})
		if answered.Code != http.StatusOK || moved.Code != http.StatusOK {
			t.Fatalf("the answer answered %d (%s) and the move %d (%s), want both written",
				answered.Code, answered.Body.String(), moved.Code, moved.Body.String())
		}
		want := []string{graphstore.KindPerson + ":alice", graphstore.KindPerson + ":carol"}
		if reached := meInboxDeliveredTo(t, database, graphstore.KindAsk, ask); !slices.Equal(reached, want) {
			t.Errorf("the answered ask's delivery stands to %v, want %v: the move came after the answer closed it", reached, want)
		}
	})
}

// TestASettlementsRestoreMovesTheAskToWhoItWaitsOnNow holds a settlement's restore of an ask it
// retracted to R2's move (docs' moveRestored): while the retraction stood
// the ask was not open, so a change of its issue's assignee moved nothing of it; once its block
// comes back the ask is open again and its delivery moves to the person it waits on now, under
// the ask.opened that records the restore, and is retracted for the person it stood to, with
// their part in the thread, which nothing else holds.
func TestASettlementsRestoreMovesTheAskToWhoItWaitsOnNow(t *testing.T) {
	t.Parallel()
	handler, database, documentService := blockAskHandlerWithDocuments(t)
	seedPeople(t, database, "dana")
	issue := createInteractionIssue(t, handler, "TEST", "A retracted ask comes back", "Context\n")
	assignIssue(t, handler, issue.Key, "bob")
	ask := writeBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID)
	opened := writeEventOf(t, database, "ask.opened", ask)
	thread := "ask:" + ask
	session := "session:" + sessionActor()["id"].(string)
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
		fmt.Sprintf("%s > person:bob @%d", thread, opened))

	removeBlockAsABrowserDoes(t, documentService, issue.PrimaryArtifactID, "decision")
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-delete")
	if retracted := readBlockAsk(t, handler, ask); retracted.State != "resolved" {
		t.Fatalf("the ask whose block was deleted is %q, want the settlement to have retracted it", retracted.State)
	}
	// The ask is not open, so the change of assignee moves nothing of it.
	assignIssue(t, handler, issue.Key, "dana")
	if under := tuplesUnder(t, database, latestEventOf(t, database, "issue.updated"), ask); len(under) != 0 {
		t.Fatalf("the change of assignee wrote %v of the retracted ask, want nothing: a move reaches open asks alone", under)
	}

	if restoring := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits",
		map[string]any{"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": transportAsk}}},
		"alice"); restoring.Code != http.StatusOK {
		t.Fatalf("write the ask block back: status=%d body=%s", restoring.Code, restoring.Body.String())
	}
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-restore")
	if back := readBlockAsk(t, handler, ask); back.State != "open" || back.WaitingOnPerson != "dana" {
		t.Fatalf("the restored ask is %q waiting on %q, want it open and waiting on dana", back.State, back.WaitingOnPerson)
	}
	restored := writeEventOf(t, database, "ask.opened", ask)
	if restored == opened {
		t.Fatalf("the settlement appended no ask.opened for the ask it restored")
	}
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
		fmt.Sprintf("%s > person:bob @%d", thread, opened),
		fmt.Sprintf("%s > person:bob @%d retracted", thread, restored),
		fmt.Sprintf("%s > person:dana @%d", thread, restored))
	requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
		fmt.Sprintf("%s > %s @%d", session, thread, opened),
		fmt.Sprintf("person:bob > %s @%d", thread, opened),
		fmt.Sprintf("person:bob > %s @%d retracted", thread, restored),
		fmt.Sprintf("person:dana > %s @%d", thread, restored))
}

// TestEveryAskCreateReachesWhoItWaitsOn holds the two creates of an ask besides createAskFor to
// the assignee arm of their create (inboxwrite.Record, the spec's R2): a settlement's
// block ask (docs' createAskBlock), whose person Record reads itself (waitingOn), and an approval
// request (openApprovalAsk), whose person its insert read (Write.WaitingOn). Each reaches the
// issue's assignee, who takes part in its thread, under its ask.opened, and its asker takes part.
func TestEveryAskCreateReachesWhoItWaitsOn(t *testing.T) {
	t.Parallel()
	t.Run("a settlement's block ask", func(t *testing.T) {
		handler, database, _ := blockAskHandlerWithDocuments(t)
		issue := createInteractionIssue(t, handler, "TEST", "A block ask waits on bob", "Context\n")
		assignIssue(t, handler, issue.Key, "bob")
		ask := writeBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID)
		opened := writeEventOf(t, database, "ask.opened", ask)
		thread := "ask:" + ask
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
			fmt.Sprintf("%s > person:bob @%d", thread, opened))
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("session:%s > %s @%d", sessionActor()["id"], thread, opened),
			fmt.Sprintf("person:bob > %s @%d", thread, opened))
	})
	t.Run("an approval request", func(t *testing.T) {
		handler, database := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "TEST", "An approval request waits on bob", "A spec")
		assignIssue(t, handler, issue.Key, "bob")
		request := requestApproval(t, handler, issue.PrimaryArtifactID)
		opened := writeEventOf(t, database, "ask.opened", request.ID)
		thread := "ask:" + request.ID
		requireRelationTuples(t, database, graphstore.TupleDeliveredTo, thread,
			fmt.Sprintf("%s > person:bob @%d", thread, opened))
		requireRelationTuples(t, database, graphstore.TupleParticipatesIn, thread,
			fmt.Sprintf("session:%s > %s @%d", sessionActor()["id"], thread, opened),
			fmt.Sprintf("person:bob > %s @%d", thread, opened))
	})
}

// writeBlockAsk writes transportAsk's block into the document as sessionActor, waits for the
// settlement that indexes it, and answers the id of the ask it opened on the issue.
func writeBlockAsk(t *testing.T, handler http.Handler, issueKey, artifactID string) string {
	t.Helper()
	written := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": transportAsk}},
	})
	if written.Code != http.StatusOK {
		t.Fatalf("write the ask block: status=%d body=%s", written.Code, written.Body.String())
	}
	awaitIndexedAskBlock(t, handler, artifactID, "decision", "Which transport?")
	return blockAskID(t, handler, issueKey, "decision")
}

// requireOneStandingDelivery fails unless ask askID waits on want (asks.AskedOfPersonExpression)
// and its delivered_to stands to want and to nobody else.
func requireOneStandingDelivery(t *testing.T, database *store.Store, askID, want string) {
	t.Helper()
	ctx := context.Background()
	var waiting string
	if err := database.Pool.QueryRow(ctx, `
		select `+askedOfPersonExpression+` from asks a`+askedOfPersonJoins+` where a.id = $1::uuid
	`, askID).Scan(&waiting); err != nil {
		t.Fatalf("read who ask %s waits on: %v", askID, err)
	}
	if waiting != want {
		t.Errorf("ask %s waits on %s, want %s", askID, waiting, want)
	}
	standing, err := readStrings(ctx, database.Pool, `
		select person from (
			`+graphstore.CurrentTupleRead(`t.to_id`, `t.to_id as person, t.asserted`,
		`t.from_kind = $1 and t.from_id = $2 and t.kind = $3 and t.to_kind = $4`)+`
		) current
		where current.asserted
		order by person
	`, graphstore.KindAsk, askID, graphstore.TupleDeliveredTo, graphstore.KindPerson)
	if err != nil {
		t.Fatalf("read the standing deliveries of ask %s: %v", askID, err)
	}
	if !slices.Equal(standing, []string{want}) {
		t.Errorf("ask %s's delivery stands to %v, want %s alone", askID, standing, want)
	}
}

// TestASessionsReplyReachesWhoStartedItsDirectConversation holds the direct-conversation arm of
// R2: a session's reply under a message on no issue that a person sent to a session reaches that
// person, whether or not their participation in the thread still stands, which
// user_threads_message never did (it returns on a message on no issue); and a session's reply
// under a message on an issue reaches nobody by that arm.
func TestASessionsReplyReachesWhoStartedItsDirectConversation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	// leave takes alice's part in the thread rooted at kind/root out of the live set, so only the
	// arm under test can reach her.
	leave := func(kind, root string) {
		writeInTx(t, database, func(tx pgx.Tx) error {
			return graphstore.ToPerson(ctx, tx, []graphstore.Tuple{{
				FromKind: graphstore.KindPerson, FromID: "alice", Kind: graphstore.TupleParticipatesIn,
				ToKind: kind, ToID: root,
				At: time.Now().UTC(), Who: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent, Asserted: false,
			}})
		})
	}

	created := postAgentMessage(t, handler, "s1", map[string]any{"body": "Can you look at the deploy?", "delivery": "steer"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("send alice's direct message: status=%d body=%s", created.Code, created.Body.String())
	}
	root := decodeBody[model.Message](t, created)
	leave(graphstore.KindMessage, root.ID)
	first := replyTo(t, handler, root.ID, "Looking.")
	if first.Code != http.StatusCreated {
		t.Fatalf("reply to alice's direct message: status=%d body=%s", first.Code, first.Body.String())
	}
	answer := decodeBody[model.Message](t, first)
	answered := writeEventOf(t, database, "message.answered", answer.ID)
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "message:"+answer.ID,
		fmt.Sprintf("message:%s > person:alice @%d", answer.ID, answered))
	// A follow-up is filed under the first reply, so its root is two steps up.
	second := replyTo(t, handler, root.ID, "It is green now.")
	if second.Code != http.StatusCreated {
		t.Fatalf("follow up in alice's direct conversation: status=%d body=%s", second.Code, second.Body.String())
	}
	followUp := decodeBody[model.Message](t, second)
	followed := writeEventOf(t, database, "message.answered", followUp.ID)
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "message:"+followUp.ID,
		fmt.Sprintf("message:%s > person:alice @%d", followUp.ID, followed))

	// A message on an issue sent to the session is no direct conversation: the reply reaches
	// nobody whose part in the thread is gone.
	issue := createInteractionIssue(t, handler, "DIRC", "Not a direct conversation", "A spec")
	onIssue := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Did the deploy finish?", "target": "session:s1", "delivery": "btw",
	}, "alice")
	leave(graphstore.KindMessage, onIssue.ID)
	replied := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+onIssue.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "It did.",
	})
	if replied.Code != http.StatusCreated {
		t.Fatalf("reply to the issue message: status=%d body=%s", replied.Code, replied.Body.String())
	}
	requireRelationTuples(t, database, graphstore.TupleDeliveredTo, "message:"+decodeBody[model.Message](t, replied).ID)
}

// countedWrite sends one request as login on a context countedRead marks.
func countedWrite(t *testing.T, handler http.Handler, method, target string, body any, login string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the counted request: %v", err)
	}
	request := httptest.NewRequestWithContext(context.WithValue(context.Background(), countedRead{}, true),
		method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", login)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// reopenIssue moves the closed issue back to todo through the route a person uses, which restores
// the asks its close withdrew.
func reopenIssue(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key,
		map[string]string{"status": "todo"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("reopen %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

// latestEventOf is the newest event of eventType, the one the write just before appended.
func latestEventOf(t *testing.T, database *store.Store, eventType string) int64 {
	t.Helper()
	var event int64
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(max(id), 0) from events where type = $1`, eventType).Scan(&event); err != nil || event == 0 {
		t.Fatalf("read the newest %s: event %d, %v", eventType, event, err)
	}
	return event
}

// tuplesUnder is every person tuple event caused with askID at either end, each written
// "<from kind>:<from id> > <to kind>:<to id>", with " retracted" after a retraction, sorted.
func tuplesUnder(t *testing.T, database *store.Store, event int64, askID string) []string {
	t.Helper()
	tuples, err := readStrings(context.Background(), database.Pool, `
		select from_kind || ':' || from_id || ' > ' || to_kind || ':' || to_id
		       || case when asserted then '' else ' retracted' end
		from graph_tuples
		where source_event = $1 and kind = any($2::text[])
		  and ((from_kind = $3 and from_id = $4) or (to_kind = $3 and to_id = $4))
		order by 1
	`, event, []string{graphstore.TupleDeliveredTo, graphstore.TupleParticipatesIn}, graphstore.KindAsk, askID)
	if err != nil {
		t.Fatalf("read the tuples event %d caused of ask %s: %v", event, askID, err)
	}
	slices.Sort(tuples)
	return tuples
}

// fallThroughProject is a project whose open asks each fall through to its default assignee.
type fallThroughProject struct {
	issue string
	asks  []string
}

// defaultFallThroughProject makes project key, whose default is alice, its creator, with an issue
// nobody holds carrying onIssue asks and a project document carrying onDocument asks, each asked
// by a session, so each waits on that default and is reached by it.
func defaultFallThroughProject(t *testing.T, handler http.Handler, key string, onIssue, onDocument int) fallThroughProject {
	t.Helper()
	issue := createInteractionIssue(t, handler, key, "Nobody holds this", "A spec")
	assignIssue(t, handler, issue.Key, "")
	project := fallThroughProject{issue: issue.Key}
	for index := range onIssue {
		project.asks = append(project.asks, createIssueAsk(t, handler, issue.Key, fmt.Sprintf("Issue question %d?", index)))
	}
	if onDocument > 0 {
		document := createProjectDocument(t, handler, key, "Notes", "# Notes\n\nA passage.\n")
		for index := range onDocument {
			response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
				"question": fmt.Sprintf("Document question %d?", index),
				"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
				"actor":    sessionActor(),
			})
			if response.Code != http.StatusCreated {
				t.Fatalf("open a question on %s's document: status=%d body=%s", key, response.Code, response.Body.String())
			}
			project.asks = append(project.asks, decodeBody[model.Ask](t, response).ID)
		}
	}
	return project
}
