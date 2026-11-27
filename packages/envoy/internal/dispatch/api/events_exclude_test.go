package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// postWriteEventTypes are the nine event types a write of a comment or a message appends, the
// ones a reader of the feed most often leaves out.
var postWriteEventTypes = []string{
	"comment.created", "comment.edited", "comment.deleted", "comment.answered",
	"message.created", "message.edited", "message.deleted", "message.answered", "message.accepted",
}

// pagedEvent is the part of an event a feed answers that these tests compare: which event it is,
// its place in its owner's sequence and its type.
type pagedEvent struct {
	ID   int64  `json:"id"`
	Seq  int    `json:"seq"`
	Type string `json:"type"`
}

// appendFeedEvents appends one event of each of types, in that order, to the owner that owner
// names (its IssueKey or its ArtifactID), as the broker appends every event, and answers them as
// appended. No payload names a post, so a read passes each through as stored.
func appendFeedEvents(t *testing.T, database *store.Store, owner model.Event, types ...string) []model.Event {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the seed transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	broker := events.NewBroker()
	appended := make([]model.Event, 0, len(types))
	for index, kind := range types {
		event := owner
		event.Type = kind
		event.Actor = model.Actor{Kind: "session", ID: "seed-session-0123456789"}
		event.Payload = map[string]any{"n": index}
		written, err := broker.Append(ctx, tx, event)
		if err != nil {
			t.Fatalf("append %s event %d: %v", kind, index, err)
		}
		appended = append(appended, written)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the seed transaction: %v", err)
	}
	return appended
}

// readFeed is the page GET target answers alice, which must be a 200.
func readFeed(t *testing.T, handler http.Handler, target string) []pagedEvent {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s, want 200", target, response.Code, response.Body.String())
	}
	return decodeBody[[]pagedEvent](t, response)
}

// withoutTypes is page less its events of types, in the order page holds them.
func withoutTypes(page []pagedEvent, types ...string) []pagedEvent {
	return slices.DeleteFunc(slices.Clone(page), func(event pagedEvent) bool {
		return slices.Contains(types, event.Type)
	})
}

// pagedEventsOf is each appended event as a feed answers it.
func pagedEventsOf(appended ...model.Event) []pagedEvent {
	page := make([]pagedEvent, len(appended))
	for index, event := range appended {
		page[index] = pagedEvent{ID: event.ID, Seq: event.Seq, Type: event.Type}
	}
	return page
}

// Each type exclude_types names is gone from the issue's feed, one type, two or all nine a post's
// writes append, and every other event stays where it was. A blank exclude_types is no filter:
// the answer is the whole feed's, byte for byte.
func TestTheEventFeedLeavesOutEachExcludedTypeAndKeepsTheRestInOrder(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "EXCL", "Excluded types", "A spec")
	var seeded []string
	for _, kind := range postWriteEventTypes {
		seeded = append(seeded, kind, "test.seeded")
	}
	appendFeedEvents(t, database, model.Event{IssueKey: new(issue.Key)}, seeded...)
	feed := "/api/v1/issues/" + issue.Key + "/events"

	whole := readFeed(t, handler, feed)
	for _, kind := range postWriteEventTypes {
		if !slices.ContainsFunc(whole, func(event pagedEvent) bool { return event.Type == kind }) {
			t.Fatalf("the whole feed holds no %s event; this case needs one of each: %+v", kind, whole)
		}
	}

	excludedSets := make([][]string, 0, len(postWriteEventTypes)+2)
	for _, kind := range postWriteEventTypes {
		excludedSets = append(excludedSets, []string{kind})
	}
	excludedSets = append(excludedSets, []string{"message.created", "comment.created"}, postWriteEventTypes)
	for _, excluded := range excludedSets {
		target := feed + "?exclude_types=" + strings.Join(excluded, ",")
		got := readFeed(t, handler, target)
		if want := withoutTypes(whole, excluded...); !slices.Equal(got, want) {
			t.Errorf("GET %s answers\n\t%+v\nwant the whole feed less %v:\n\t%+v", target, got, excluded, want)
		}
	}

	unfiltered := dispatchRequest(t, handler, http.MethodGet, feed, nil, "alice")
	if unfiltered.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s, want 200", feed, unfiltered.Code, unfiltered.Body.String())
	}
	for _, blank := range []string{"?exclude_types=", "?exclude_types=%20"} {
		response := dispatchRequest(t, handler, http.MethodGet, feed+blank, nil, "alice")
		if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), unfiltered.Body.Bytes()) {
			t.Errorf("GET %s%s: status=%d body=%s, want 200 and the whole feed's body %s",
				feed, blank, response.Code, response.Body.String(), unfiltered.Body.String())
		}
	}
}

// The filter is in the statement, before its limit counts the page: ten excluded events newer
// than the three kept ones leave a page of five holding all three, where a filter after the read
// would answer none of them. after, before, order and ids keep their positions and their order
// over the events that are left.
func TestTheEventFeedExcludesTypesBeforeItCountsThePage(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	key := createPlainIssue(t, handler, "EXCLP")
	feed := "/api/v1/issues/" + key + "/events"
	opened := readFeed(t, handler, feed)
	if len(opened) != 1 || opened[0].Type != "issue.created" {
		t.Fatalf("a new issue's feed is %+v; this case needs its issue.created alone", opened)
	}

	seeded := appendFeedEvents(t, database, model.Event{IssueKey: new(key)},
		"test.seeded", "comment.created", "test.seeded", "comment.created", "test.seeded",
		"comment.created", "comment.created", "comment.created", "comment.created", "comment.created",
		"comment.created", "comment.created", "comment.created", "comment.created", "comment.created")
	first, second, third := seeded[0], seeded[2], seeded[4]
	newest := seeded[len(seeded)-1]

	// The control: unfiltered, the newest five events are all comments.
	for _, event := range readFeed(t, handler, feed+"?order=desc&limit=5") {
		if event.Type != "comment.created" {
			t.Fatalf("the newest five events hold %+v; this case needs five comments above the kept events", event)
		}
	}

	const both = "&exclude_types=comment.created,issue.created"
	for _, row := range []struct {
		query string
		want  []model.Event
	}{
		{"?order=desc&limit=5" + both, []model.Event{third, second, first}},
		{"?order=desc&limit=2" + both, []model.Event{third, second}},
		{fmt.Sprintf("?before=%d&limit=2", newest.Seq+1) + both, []model.Event{third, second}},
		{fmt.Sprintf("?before=%d&limit=1", third.Seq) + both, []model.Event{second}},
		{fmt.Sprintf("?before=%d&limit=2", second.Seq) + both, []model.Event{first}},
		{"?order=asc&limit=2" + both, []model.Event{first, second}},
		{fmt.Sprintf("?after=%d&limit=1", first.Seq) + both, []model.Event{second}},
		{fmt.Sprintf("?after=%d", second.Seq) + both, []model.Event{third}},
		{fmt.Sprintf("?after=%d", third.Seq) + both, []model.Event{}},
		{fmt.Sprintf("?ids=%d,%d,%d,%d", newest.ID, third.ID, seeded[1].ID, first.ID) + both, []model.Event{first, third}},
		{"?limit=4&exclude_types=comment.created", []model.Event{{ID: opened[0].ID, Seq: opened[0].Seq, Type: opened[0].Type}, first, second, third}},
	} {
		target := feed + row.query
		if got, want := readFeed(t, handler, target), pagedEventsOf(row.want...); !slices.Equal(got, want) {
			t.Errorf("GET %s answers\n\t%+v\nwant\n\t%+v", target, got, want)
		}
	}
}

// A document's feed is paged by the same options: exclude_types leaves its types out of the page,
// before the limit counts it, as it does on an issue's feed.
func TestADocumentsEventFeedLeavesOutTheExcludedTypes(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createAdviceProject(t, handler, "EXCLA")
	document := uploadDocument(t, handler, "/api/v1/projects/EXCLA", "notes.md", "# Notes\n\nOne.\n")
	appendFeedEvents(t, database, model.Event{ArtifactID: new(document.Artifact.ID)},
		"comment.created", "test.seeded", "comment.edited", "comment.created", "test.seeded",
		"comment.created", "comment.created", "comment.created")
	feed := "/api/v1/artifacts/" + document.Artifact.ID + "/events"

	whole := readFeed(t, handler, feed)
	kept := withoutTypes(whole, "comment.created")
	if len(kept) == len(whole) || len(kept) < 3 {
		t.Fatalf("the document's feed is %+v; this case needs comments and at least three other events", whole)
	}
	target := feed + "?exclude_types=comment.created"
	if got := readFeed(t, handler, target); !slices.Equal(got, kept) {
		t.Errorf("GET %s answers\n\t%+v\nwant the whole feed less its comments:\n\t%+v", target, got, kept)
	}
	newestKept := slices.Clone(kept)
	slices.Reverse(newestKept)
	target = feed + "?order=desc&limit=2&exclude_types=comment.created"
	if got, want := readFeed(t, handler, target), newestKept[:2]; !slices.Equal(got, want) {
		t.Errorf("GET %s answers\n\t%+v\nwant the two newest events that are no comment:\n\t%+v", target, got, want)
	}
}

// Both feeds refuse an exclude_types that names a type Dispatch does not write, holds an empty
// item in the list or names more than 50, with 400 INVALID_QUERY; 50 known types are read.
func TestBothEventFeedsRefuseABadExcludeTypes(t *testing.T) {
	t.Parallel()
	// parseEventExcludeTypes finds a type by binary search, which needs the list sorted.
	if !slices.IsSorted(contracts.DispatchEventTypes) {
		t.Fatalf("contracts.DispatchEventTypes is not sorted: %v", contracts.DispatchEventTypes)
	}
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "EXCLB", "Bad excluded types", "A spec")
	createAdviceProject(t, handler, "EXCLC")
	document := uploadDocument(t, handler, "/api/v1/projects/EXCLC", "notes.md", "# Notes\n\nOne.\n")
	repeated := func(count int) string {
		return strings.TrimSuffix(strings.Repeat("comment.created,", count), ",")
	}
	for _, feed := range []string{
		"/api/v1/issues/" + issue.Key + "/events",
		"/api/v1/artifacts/" + document.Artifact.ID + "/events",
	} {
		for _, value := range []string{
			"not.a.kind",
			"comment.created,not.a.kind",
			"COMMENT.CREATED",
			"comment.created,,comment.edited",
			",comment.created",
			"comment.created,",
			"comment.created,%20",
			repeated(51),
		} {
			target := feed + "?exclude_types=" + value
			response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
			if response.Code != http.StatusBadRequest || responseCode(t, response) != "INVALID_QUERY" {
				t.Errorf("GET %s: status=%d body=%s, want 400 INVALID_QUERY", target, response.Code, response.Body.String())
			}
		}
		target := feed + "?exclude_types=" + repeated(50)
		if response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"); response.Code != http.StatusOK {
			t.Errorf("GET %s: status=%d body=%s, want 200", target, response.Code, response.Body.String())
		}
	}
}

// The feed's validator covers exclude_types: a filtered page and the whole feed, or two different
// sets of types, never share an ETag, and a reader holding one is answered the other whole, never
// the bytes kept for it. Every spelling of one set, in any order and with repeats, is one page
// under one ETag, which a reader holding it is answered 304 for.
func TestTheEventFeedsETagCoversItsExcludedTypes(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	_, _, feed := listTargets(t, handler, "EXCLE", 3)
	etagOf := func(query string) string {
		t.Helper()
		return listETagOf(t, listRead(t, handler, feed+query, listReadOptions{trees: true}), "GET "+feed+query)
	}

	distinct := map[string]string{}
	for _, query := range []string{
		"",
		"?exclude_types=comment.created",
		"?exclude_types=issue.created",
		"?exclude_types=comment.created,issue.created",
	} {
		etag := etagOf(query)
		for earlier, held := range distinct {
			if held == etag {
				t.Errorf("GET %s%s answers the ETag of GET %s%s (%s): exclude_types is not in the validator",
					feed, query, feed, earlier, etag)
			}
		}
		distinct[query] = etag
	}

	set := distinct["?exclude_types=comment.created,issue.created"]
	for _, query := range []string{
		"?exclude_types=issue.created,comment.created",
		"?exclude_types=comment.created,comment.created,issue.created",
		"?exclude_types=%20issue.created%20,comment.created",
	} {
		if etag := etagOf(query); etag != set {
			t.Errorf("GET %s%s answers the ETag %s, want %s, the ETag of the same set spelled comment.created,issue.created",
				feed, query, etag, set)
		}
		target := feed + query
		requireNotModified(t, listRead(t, handler, target, listReadOptions{trees: true, etag: set}), set,
			"GET "+target+" holding the ETag of the same set")
	}
	if etag := etagOf("?exclude_types="); etag != distinct[""] {
		t.Errorf("GET %s?exclude_types= answers the ETag %s, want the whole feed's %s", feed, etag, distinct[""])
	}

	target := feed + "?exclude_types=comment.created"
	crossed := listRead(t, handler, target, listReadOptions{trees: true, etag: distinct[""]})
	if crossed.Code != http.StatusOK {
		t.Fatalf("GET %s holding the whole feed's ETag: status=%d, want 200", target, crossed.Code)
	}
	for _, event := range decodeBody[[]pagedEvent](t, crossed) {
		if event.Type == "comment.created" {
			t.Errorf("GET %s holding the whole feed's ETag answers a comment.created event: %+v", target, event)
		}
	}
}
