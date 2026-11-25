package api

import (
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The pile TestAnOwnReplyUnderAnUnansweredAgentMessageKeepsTheDismissedPageWhole seeds with the
// scale seed's own helpers (inbox_dismissed_scale_seed_test.go): more dismissed Agent threads and
// more dismissed Reply threads than one Dismissed page holds, so a read of either kind alone fills
// a page as a read of both kinds does.
const (
	unansweredPageAgents  = myRepliesDismissedPage + 5
	unansweredPageReplies = myRepliesDismissedPage + 10
)

// c-dismissed-page's own reply (the own-answer rule, R1): a person's own reply under a one-off
// Agent message that no session has answered writes no Agent dismissal. The reads draw an Agent row
// from the session replies of its root's thread, through an inner lateral (myAgentRowFacts), so a
// root no session answered is no Agent row. alice holds more dismissed threads than one Dismissed
// page, sends s1 a one-off message no session answers, and replies under it as the Agents page
// sends a reply, with in_reply_to naming the root. The reply writes no user_agent_thread_dismissed
// row, and no dismissed tuple of alice's stands on the root. The Dismissed view's first page, read
// through GET /api/v1/me/inbox and through GET /api/v1/me/replies, the latter with no kind, which
// reads the Reply rows alone, and with kind=agent, the Agent rows alone, is full and names a
// next_cursor, and its cursor walks every dismissed thread it lists once, newest dismissal first.
// A mark of that root would be the newest of all: each read cuts the marks to the page before the
// lateral drops the root (meInboxDismissedKeys, myAgentDismissedPageCut), so the page would answer
// one row fewer than its cut took, and the Inbox's page and the Replies route's Agent page would
// name no cursor.
func TestAnOwnReplyUnderAnUnansweredAgentMessageKeepsTheDismissedPageWhole(t *testing.T) {
	t.Parallel()
	// A listener that holds s1 live, so alice's message and her reply reach it; s1 never answers.
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createInteractionIssue(t, handler, "UNAN", "A pile of dismissed threads", "A spec")
	shape := &dismissedScaleShape{
		name: "a pile past one page", handler: handler, database: database, issue: issue.Key,
		dismissedReplies: unansweredPageReplies, dismissedAgents: unansweredPageAgents,
	}
	dismissedScaleCommentPairs(t, shape, dismissedScalePileLabel, unansweredPageReplies, dismissedScalePileAge)
	markDismissedScaleThreads(t, shape, unansweredPageReplies)
	dismissedScaleMessagePairs(t, shape, dismissedScaleAgentPileLabel, unansweredPageAgents, dismissedScaleAgentAge)
	markDismissedScaleAgentThreads(t, shape, unansweredPageAgents)
	// The dismissed threads the views list, read before alice's reply, so what the reply wrote is
	// no part of what the walks are held to.
	want := dismissedNarrowingOrder(t, shape)
	if len(want) != unansweredPageReplies+unansweredPageAgents {
		t.Fatalf("the seed dismissed %d threads, want %d", len(want), unansweredPageReplies+unansweredPageAgents)
	}
	wantAgents := slices.DeleteFunc(slices.Clone(want), func(node string) bool {
		return !strings.HasPrefix(node, graphstore.KindMessage+":")
	})
	wantReplies := slices.DeleteFunc(slices.Clone(want), func(node string) bool {
		return strings.HasPrefix(node, graphstore.KindMessage+":")
	})

	created := postAgentMessage(t, handler, "s1", map[string]any{"body": "Is the canary still green?", "delivery": "btw"}, dismissedScaleLogin)
	if created.Code != http.StatusCreated {
		t.Fatalf("message s1: status=%d body=%s", created.Code, created.Body.String())
	}
	root := decodeBody[model.Message](t, created)
	reply := ownDismissAgentReply(t, handler, root.ID, "And where is its dashboard?", "")
	requireNoOwnDismissal(t, database, ownWrite{
		person: dismissedScaleLogin, rootKind: graphstore.KindMessage, rootID: root.ID,
		event: writeEventOf(t, database, "message.answered", reply.ID), agent: true,
	})

	dismissed := url.Values{"state": {myRepliesStateDismissed}}
	inbox := func(query url.Values) func(cursor string) ([]string, string) {
		return func(cursor string) ([]string, string) {
			paged := maps.Clone(query)
			if cursor != "" {
				paged.Set("cursor", cursor)
			}
			answer := readMeInboxAs(t, handler, dismissedScaleLogin, paged)
			return meInboxNodes(answer), answer.NextCursor
		}
	}
	replies := func(query url.Values) func(cursor string) ([]string, string) {
		return func(cursor string) ([]string, string) {
			paged := maps.Clone(query)
			if cursor != "" {
				paged.Set("cursor", cursor)
			}
			answer := getMyReplies(t, handler, dismissedScaleLogin, "?"+paged.Encode())
			nodes := make([]string, 0, len(answer.Rows))
			for _, row := range answer.Rows {
				kind := row.Kind
				if kind == myReplyKindAgent {
					kind = graphstore.KindMessage
				}
				nodes = append(nodes, kind+":"+row.ThreadID)
			}
			return nodes, answer.NextCursor
		}
	}

	// GET /api/v1/me/inbox narrows nothing by the kind of row: its kind names relation kinds, and
	// keeps the entries holding a relation of one of them to the person (meInboxBuild's readKind).
	// No relation is of kind "agent", so the Dismissed view narrowed to it answers no entry and no
	// cursor, before this reply as after it. The Agent rows alone are GET /api/v1/me/replies's
	// kind=agent, which a walk below reads.
	narrowed := readMeInboxAs(t, handler, dismissedScaleLogin, url.Values{"state": {myRepliesStateDismissed}, "kind": {myReplyKindAgent}})
	if len(narrowed.Entries) != 0 || narrowed.NextCursor != "" {
		t.Errorf("GET /api/v1/me/inbox?state=dismissed&kind=agent answers %v and the cursor %q, want no entry and no cursor: no relation is of kind agent",
			meInboxNodes(narrowed), narrowed.NextCursor)
	}

	walkWholeDismissedView(t, "GET /api/v1/me/inbox?state=dismissed", inbox(dismissed), want)
	// GET /api/v1/me/replies with no kind reads the Reply rows alone (parseMyRepliesKinds).
	walkWholeDismissedView(t, "GET /api/v1/me/replies?state=dismissed", replies(dismissed), wantReplies)
	walkWholeDismissedView(t, "GET /api/v1/me/replies?state=dismissed&kind=agent",
		replies(url.Values{"state": {myRepliesStateDismissed}, "kind": {myReplyKindAgent}}), wantAgents)
}

// walkWholeDismissedView reads one Dismissed read from its first page to the page that names no
// cursor, page answering the threads of one page as "kind:id" and the cursor it names, from cursor
// ("" for the first page). The first page is full and names a cursor, no page is empty, and the
// walk reads want, every thread once, in want's order.
func walkWholeDismissedView(t *testing.T, name string, page func(cursor string) ([]string, string), want []string) {
	t.Helper()
	read := []string{}
	cursor := ""
	for number := 1; ; number++ {
		if number > len(want)+1 {
			t.Fatalf("%s read %d threads over %d pages and still names a cursor", name, len(read), number-1)
		}
		nodes, next := page(cursor)
		if number == 1 && (len(nodes) != myRepliesDismissedPage || next == "") {
			t.Fatalf("%s's first page answers %d threads and the cursor %q, want a full page of %d and a cursor, since %d threads are dismissed: %v",
				name, len(nodes), next, myRepliesDismissedPage, len(want), nodes)
		}
		if len(nodes) == 0 {
			t.Errorf("%s's page %d answers no thread after %d of the %d dismissed: the page before it named a cursor with no thread past it",
				name, number, len(read), len(want))
		}
		read = append(read, nodes...)
		if next == "" {
			break
		}
		cursor = next
	}
	if !slices.Equal(read, want) {
		t.Fatalf("%s's cursor walk read %d threads, want every one of the %d dismissed once, newest dismissal first: read %v, want %v",
			name, len(read), len(want), read, want)
	}
}
