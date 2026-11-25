//go:build twobuild

// The keyword half of the two-build suite: what the keyword fill of the build before this one
// writes of a long comment this build wrote, and what this build's fill makes of it
// (twobuild_harness_test.go holds the harness every half starts its builds with).

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
)

// TestTheFirstBodyRepairRebuildsACutVersionThePreviousBuildsFillWrote runs this build with its
// keyword fill off, so it takes no hold, and writes through it a comment longer than an event
// carries, then an edit of it, so the comment's row no longer holds its first body. The edit keeps
// the body it replaced in the comment's body history (post_body_history), which the previous
// build's fill reads before the comment's blocks (textversions' historyBody), and which an edit a
// build before the body history wrote did not keep; the test takes that one row out, so the
// comment stands as such an edit leaves it, the one case whose first body the previous build
// leaves to revision 1 of its blocks. The previous build then serves with its fill on and decides
// the comment.created version: its reader of revision 1 takes a block as the comment's own only
// where the block's row holds the event's created_at, the transaction's start, and this build
// dates the row by the comment's own moment, so the previous build keeps the event's cut text.
// This build's census of the store counts that version among the cut first bodies revision 1
// proves. Once the previous build has stopped, this build serves with its fill on: when it has
// taken the hold and its first pass ends, its first-body repair rebuilds the version from revision
// 1, its body revision 1's blocks in their order, from blocks, and the census then counts none.
func TestTheFirstBodyRepairRebuildsACutVersionThePreviousBuildsFillWrote(t *testing.T) {
	builds := newTwoBuilds(t)
	current := builds.startCurrent(t, map[string]string{"DISPATCH_SEARCH_FILL": "off"})
	project := uniqueProjectKey(t, "KWRD")
	current.createProject(t, project, "Two builds' keyword versions")
	issue, _ := current.createIssueIn(t, project, "Rotate the signing keys", "The rotation plan.\n")
	first := twoBuildLongCommentBody()
	written := current.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
		jsonBody(t, map[string]any{"body": first}), http.StatusCreated)
	var comment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(written, &comment); err != nil || comment.ID == "" {
		t.Fatalf("write the long comment: %v: %s", err, written)
	}
	current.call(t, http.MethodPatch, "/api/v1/comments/"+comment.ID,
		jsonBody(t, map[string]any{"body": "A new opening paragraph, written once the rotation was planned.\n\n" + first}), http.StatusOK)
	event := commentCreatedEventOf(t, builds, comment.ID)
	forgetCommentBodyHistory(t, builds, comment.ID)

	builds.startPrevious(t, nil)
	if from, _ := awaitKeywordVersion(t, builds, comment.ID, event, "", 2*time.Minute); from != "cut" {
		t.Fatalf("the %s's fill wrote the comment.created version from %q, want \"cut\": its reader takes no block this build wrote as the comment's own",
			builds.previousName(), from)
	}
	if counted := cutFirstBodiesCensus(t, builds); counted != 1 {
		t.Fatalf("the census counts %d cut first bodies revision 1 proves, want the one the %s's fill wrote", counted, builds.previousName())
	}

	stop(t, builds.previous())
	stop(t, current)
	builds.startCurrent(t, nil)
	// awaitKeywordVersion answers only once the version holds its body from blocks.
	_, body := awaitKeywordVersion(t, builds, comment.ID, event, "blocks", 2*time.Minute)
	blocks := itemblocks.Split(first)
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		texts = append(texts, block.Text)
	}
	if want := strings.Join(texts, "\n\n"); body != want {
		t.Fatalf("after this build took the hold the version holds %d bytes from blocks, want revision 1's %d bytes", len(body), len(want))
	}
	if counted := cutFirstBodiesCensus(t, builds); counted != 0 {
		t.Fatalf("after the repair the census counts %d cut first bodies revision 1 proves, want none", counted)
	}
}

// twoBuildLongCommentBody is a body over the 32,000 UTF-16 units an event carries whole: three
// paragraphs of about 10,500 units each, which together fit in an event, and a list after them,
// which does not. Each block is under the create route's block bound.
func twoBuildLongCommentBody() string {
	paragraph := func(topic string) string {
		var b strings.Builder
		for number := 1; b.Len() < 10_500; number++ {
			fmt.Fprintf(&b, "The %s rotation step %d moves one signing key to its successor. ", topic, number)
		}
		return strings.TrimSpace(b.String())
	}
	var list strings.Builder
	for number := 1; number <= 60; number++ {
		if number > 1 {
			list.WriteString("\n")
		}
		fmt.Fprintf(&list, "- delta check %d confirms the verifier reads the new key", number)
	}
	return strings.Join([]string{paragraph("alpha"), paragraph("bravo"), paragraph("charlie"), list.String()}, "\n\n")
}

// commentCreatedEventOf is the id of the comment.created event of comment id.
func commentCreatedEventOf(t *testing.T, builds *twoBuilds, id string) int64 {
	t.Helper()
	var event int64
	if err := builds.database.Pool.QueryRow(context.Background(),
		`select id from events where type = 'comment.created' and payload->>'id' = $1`, id).Scan(&event); err != nil {
		t.Fatalf("read the comment.created event of comment %s: %v", id, err)
	}
	return event
}

// forgetCommentBodyHistory takes out the one body comment id's body history holds, the body its one
// edit replaced, so the comment stands as an edit a build before the body history wrote leaves it:
// a comment.edited with no body kept. It fails the test unless the history held exactly that row.
func forgetCommentBodyHistory(t *testing.T, builds *twoBuilds, id string) {
	t.Helper()
	tag, err := builds.database.Pool.Exec(context.Background(), `delete from post_body_history where comment_id = $1`, id)
	if err != nil {
		t.Fatalf("take the body history of comment %s out: %v", id, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("the body history of comment %s held %d bodies, want the one its edit kept", id, tag.RowsAffected())
	}
}

// awaitKeywordVersion waits up to within for the keyword version event started of comment id to
// stand, and, where want is not empty, to hold its body from want, and answers where its body came
// from and the body. It fails the test when the wait ends first.
func awaitKeywordVersion(t *testing.T, builds *twoBuilds, id string, event int64, want string, within time.Duration) (from, body string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		err := builds.database.Pool.QueryRow(context.Background(), `
			select body_from, body from search_text_versions where kind = 'comment' and id = $1 and event_id = $2
		`, id, event).Scan(&from, &body)
		if err == nil && (want == "" || from == want) {
			return from, body
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s the keyword version of event %d of comment %s holds its body from %q (%v), want %q", within, event, id, from, err, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// cutFirstBodiesLine is the census's line of the cut first bodies revision 1 proves.
var cutFirstBodiesLine = regexp.MustCompile(`(?m)^census: store keyword_cut_first_bodies=([0-9,]+) `)

// cutFirstBodiesCensus runs this build's store census against the pair's database and answers the
// count of the cut first bodies revision 1 proves it prints.
func cutFirstBodiesCensus(t *testing.T, builds *twoBuilds) int64 {
	t.Helper()
	census := runCurrentCommand(t, builds, []int{0, 1}, "census", "--store")
	found := cutFirstBodiesLine.FindStringSubmatch(census)
	if found == nil {
		t.Fatalf("the census printed no keyword_cut_first_bodies line:\n%s", census)
	}
	count, err := strconv.ParseInt(strings.ReplaceAll(found[1], ",", ""), 10, 64)
	if err != nil {
		t.Fatalf("read the census's count %q: %v", found[1], err)
	}
	return count
}
