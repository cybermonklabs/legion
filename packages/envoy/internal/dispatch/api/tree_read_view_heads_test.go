package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// viewHeadsIssue is an issue of the project project whose spec holds the requirements r1 and r2,
// created as alice on a server of its own whose pool its head rig traces (headRig), with the
// documents settled and the block index run; and write, which writes a comment on it and answers
// its id: a thread's first comment, on the text of the requirement block, where replyTo is "", and
// otherwise a reply to replyTo, as login, or as an agent's session where login is "".
func viewHeadsIssue(t *testing.T, project string) (headRig, string, func(block, body, login, replyTo string) string) {
	t.Helper()
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	handler, _, deps, _ := newRegisteredTestServer(t, testServerOptions{store: traced, settle: time.Hour})
	quotes := map[string]string{"r1": "The board shows each ask once.", "r2": "The board orders the asks by urgency."}
	issue := createInteractionIssue(t, handler, project, "A view's comments on text",
		requirementMarkdown("r1", quotes["r1"])+"\n"+requirementMarkdown("r2", quotes["r2"]))
	key := issue.Key
	r := newHeadRig(t, handler, database, counter, key)
	r.requireMarked(key)
	settleIssue(t, deps.Docs.(*docs.Service), key)
	reconcileBlockIndexPass(t, deps)
	r.after("issue create", key)
	write := func(block, body, login, replyTo string) string {
		t.Helper()
		input := map[string]any{"body": body}
		if replyTo == "" {
			input["anchor"] = map[string]any{"artifact": issue.PrimaryArtifactID, "block_id": block, "quote": quotes[block]}
		} else {
			input["reply_to"] = replyTo
		}
		if login == "" {
			input["actor"] = sessionActor()
		}
		return createThreadComment(t, handler, key, input, login).ID
	}
	return r, key, write
}

// requireViewsAsWalk holds every read of key's tree to its walk (headRig.after) after the write of
// class, and each page of each view's comments_before as alice and as bob
// (requireViewCommentPagesAsWalk).
func requireViewsAsWalk(t *testing.T, r headRig, class, key string) {
	t.Helper()
	r.after(class, key)
	for _, view := range issueTreeViews {
		for _, login := range []string{"alice", "bob"} {
			requireViewCommentPagesAsWalk(t, r, key, view, login)
		}
	}
}

// TestAViewReadOfFewerThreadsThanItsChoiceAnswersAsTheWalk holds each view's head read of an issue
// of 300 threads on text, each a first comment and its reply written one after the other, on r1 and
// on r2 by turns, every third an agent's that no person wrote in, to the walk of the tuples, as
// alice and as bob, on each page of each view's comments_before. The issue's first comments on text
// are fewer than a view's first read of its rows reads (viewTextChoice), and its comments more, so
// that read takes the comments of their threads heads first (viewTextCommentsStatement) and keeps
// the newest of them; the spec view carries more than its page of those, so it answers from that
// read alone. A read that kept older comments than the newest would answer the spec view's first
// page from comments older than its walk's.
func TestAViewReadOfFewerThreadsThanItsChoiceAnswersAsTheWalk(t *testing.T) {
	t.Parallel()
	r, key, write := viewHeadsIssue(t, "VTH")
	for index := range 300 {
		login, block := "alice", "r1"
		if index%3 == 0 {
			login = ""
		}
		if index%2 == 0 {
			block = "r2"
		}
		head := write(block, fmt.Sprintf("Thread %d on the board.", index+1), login, "")
		write("", fmt.Sprintf("A reply in thread %d.", index+1), login, head)
	}
	requireViewsAsWalk(t, r, "300 threads on text of two comments each", key)
}

// TestAViewReadOfRepliesNewerThanTheirThreadAnswersAsTheWalk holds each view's head read of an
// issue to the walk of the tuples, as alice and as bob, on each page of each view's
// comments_before, where the 60 newest comments on text are replies in a person's thread on r1
// whose first comment is older than the 380 threads on r1 written between them. A view's first
// read of its rows chooses those 60 and 340 of the 380 (viewTextChoice): it reads the rows of the
// thread's first comment too (chooseViewText), so the spec view carries the replies under it as
// the walk does, and its first page is the 60 replies and the 140 newest of the threads.
func TestAViewReadOfRepliesNewerThanTheirThreadAnswersAsTheWalk(t *testing.T) {
	t.Parallel()
	r, key, write := viewHeadsIssue(t, "VTR")
	first := write("r1", "The oldest thread on the board.", "alice", "")
	for index := range 380 {
		write("r1", fmt.Sprintf("Thread %d on the board.", index+1), "alice", "")
	}
	for index := range 60 {
		write("", fmt.Sprintf("A late reply %d in the oldest thread.", index+1), "alice", first)
	}
	requireViewsAsWalk(t, r, "an old thread whose replies are the newest comments on text", key)
}
