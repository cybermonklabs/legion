package api

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
)

func TestCommentThreadPageAnswersAnETagOverEveryParameter(t *testing.T) {
	t.Parallel()
	h, db := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, h, "PROJ", "Comment page validators", "The selected text.")
	list := commentListOf(issue.Key)
	create := func(input map[string]any, n int) string {
		id := createThreadComment(t, h, issue.Key, input, "alice").ID
		timeFixtureEntry(t, db, "comments", id, n)
		return id
	}
	anchored := create(map[string]any{"body": "On the spec", "anchor": map[string]any{"artifact": "spec", "quote": "selected text"}}, 1)
	create(map[string]any{"body": "A first note"}, 2)
	create(map[string]any{"body": "A second note"}, 3)
	first := list + "?order=activity"
	// A limit of one yields a position that actually divides the fixture.
	one := pageJSON(t, h, list, "&limit=1")
	fields := []string{"limit=1", "before=" + url.QueryEscape(*one.Before), "author=alice", "person=in", "artifact=" + issue.PrimaryArtifactID}
	targets := []string{first}
	for i, field := range fields {
		targets = append(targets, first+"&"+field)
		for _, other := range fields[i+1:] {
			targets = append(targets, first+"&"+field+"&"+other)
		}
	}
	type answer struct {
		etag string
		body []byte
	}
	answers := map[string]answer{}
	for _, target := range append([]string{list}, targets...) {
		response := listRead(t, h, target, listReadOptions{trees: true})
		etag := listETagOf(t, response, target)
		if target != list {
			decodeThreadPage(t, response.Body.Bytes(), response.Code, target)
		}
		answers[target] = answer{etag, response.Body.Bytes()}
		requireNotModified(t, listRead(t, h, target, listReadOptions{trees: true, etag: etag}), etag, target)
	}
	for _, target := range targets {
		// Send the tag from a URL that differs by one parameter, including order.
		base := list
		if i := strings.LastIndexByte(target, '&'); i != -1 {
			base = target[:i]
		}
		want := answers[target]
		response := listRead(t, h, target, listReadOptions{trees: true, etag: answers[base].etag})
		if etag := listETagOf(t, response, target+" holding the ETag of "+base); etag != want.etag || etag == answers[base].etag || !bytes.Equal(response.Body.Bytes(), want.body) {
			t.Fatalf("%s holding the ETag of %s did not answer its own page", target, base)
		}
	}
	seen := map[string]string{}
	for _, target := range append([]string{list}, targets...) {
		if other, exists := seen[answers[target].etag]; exists {
			t.Errorf("%s and %s share an ETag", target, other)
		}
		seen[answers[target].etag] = target
	}
	etag := answers[first].etag
	root := create(map[string]any{"body": "A new thread"}, 4)
	etag = requireMovedValidator(t, h, first, etag, "a new comment")
	create(map[string]any{"body": "A reply to an old thread", "reply_to": anchored}, 5)
	etag = requireMovedValidator(t, h, first, etag, "a new reply")
	seq := issueLastSeq(t, db, issue.Key)
	legionExec(t, db, `insert into comment_deliveries (comment_id,target,attempt,delivery,session_id,state,claimed_at)
  values ($1,'session:s1',1,'steer','s1','pending',now())`, root)
	if issueLastSeq(t, db, issue.Key) != seq {
		t.Fatal("the delivery appended an event")
	}
	requireMovedValidator(t, h, first, etag, "a comment delivery claim")
	response := listRead(t, h, first, listReadOptions{trees: true})
	if _, held := valueAt(t, response, "threads.1.comment.deliveries.0"); !held {
		t.Fatal("the page did not show the delivery")
	}
}
