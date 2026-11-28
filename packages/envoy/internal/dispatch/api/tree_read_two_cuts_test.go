package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// twoCutsForms are the reads TestTheTwoCutsAnswerAsTheUncutWalk replays in each state of the store,
// each as each reader of that state, from its first page: the read with no view, each view, and two
// kind filters, the conversation's kinds (R30) and a check with its results. This list is the file
// of reads c-tree-read-bound names.
var twoCutsForms = []struct{ name, query string }{
	{"the read with no view", ""},
	{"?view=spec", "?view=" + graphstore.QuerySpec},
	{"?view=verification", "?view=" + graphstore.QueryVerification},
	{"?view=plan", "?view=" + graphstore.QueryPlan},
	{"?view=decisions", "?view=" + graphstore.QueryDecisions},
	{"the read with the conversation's kinds", kindFilterQuery(kindFilterConversation...)},
	{"kind=check&kind=result", kindFilterQuery(graphstore.BlockTypeCheck, graphstore.KindResult)},
}

// twoCutsNodeLimit is the node limit of the replay's second server, at which the page window cuts
// real pages.
const twoCutsNodeLimit = 2

// twoCutsPageBound is the most pages the replay follows one read through at twoCutsNodeLimit.
const twoCutsPageBound = 5_000

// twoCutsReplay is the replay of TestTheTwoCutsAnswerAsTheUncutWalk: the second server, over the
// same store, whose node limit is twoCutsNodeLimit, made at the first state, and how many states,
// reads at the default node limit and pages at twoCutsNodeLimit it held.
type twoCutsReplay struct {
	t                    *testing.T
	small                http.Handler
	smallServed          *server
	states, reads, pages int
}

// TestTheTwoCutsAnswerAsTheUncutWalk is c-tree-read-bound's replay: R8's two cuts - each loader of
// the read once, and a page that reads and builds only its own window - keep every byte of each
// answer. It runs serially, as TestTheHeadProjectionAnswersAsTheWalk does, whose states it builds
// through the same writes: those states hold pg_locks waits to deadlines of their own.
//
// In each state it reads each of twoCutsForms as each reader of the state twice at the same cut:
// through the route, with both cuts, and through the un-cut walk (readUncutIssueTree), with the
// issue's mark taken away for that read's length, so it walks the tuples. It reads so at the
// server's default node limit, and again on a second server over the same store whose node limit is
// twoCutsNodeLimit (testServerOptions.issueTreePageSize), where it follows each cursor to the end,
// so the page window cuts real pages. Each answer of the route equals the un-cut walk's answer, byte
// for byte, no member removed from either; else the test prints the state, the read, the page and
// the first byte where the two answers differ.
func TestTheTwoCutsAnswerAsTheUncutWalk(t *testing.T) {
	replay := &twoCutsReplay{t: t}
	requireHeadProjectionStates(t, replay.observe)
	t.Logf("c-tree-read-bound: the replay held %d states of the store: %d reads of the route at its default node limit and %d pages at a node limit of %d, "+
		"each the answer the un-cut walk answers at the same cut, byte for byte", replay.states, replay.reads, replay.pages, twoCutsNodeLimit)
	if replay.reads == 0 || replay.pages == 0 {
		t.Errorf("the replay read %d reads and %d pages, want both", replay.reads, replay.pages)
	}
}

// observe replays each of twoCutsForms in the state the head projection's test just held, as each
// of logins: from the first page at the default node limit on r's own server, and every page at
// twoCutsNodeLimit on the second server.
func (replay *twoCutsReplay) observe(r headRig, class, key string, logins []string) {
	t := replay.t
	t.Helper()
	if replay.small == nil {
		replay.small, _, _, replay.smallServed = newRegisteredTestServer(t, testServerOptions{store: r.database, settle: time.Hour, issueTreePageSize: twoCutsNodeLimit})
	}
	replay.states++
	for _, login := range logins {
		for _, form := range twoCutsForms {
			read := fmt.Sprintf("%s of %s as %s", form.name, key, login)
			replay.reads++
			replay.page(r, r.handler, r.served, class, key, read, form.query, login, "", 1)
			cursor := ""
			for page := 1; ; page++ {
				if page > twoCutsPageBound {
					t.Errorf("the replay, after the %s write: %s still answers a cursor after %d pages of %d nodes", class, read, twoCutsPageBound, twoCutsNodeLimit)
					break
				}
				replay.pages++
				next := replay.page(r, replay.small, replay.smallServed, class, key, read+fmt.Sprintf(" at a node limit of %d", twoCutsNodeLimit), form.query, login, cursor, page)
				if next == "" {
					break
				}
				cursor = next
			}
		}
	}
}

// page reads the page of query that cursor names ("" for the first) of key's tree as login through
// the route handler mounts and through the un-cut walk of served, at one cut - a pair of reads the
// store moved between is read again - and fails, naming the state, the read, the page and the first
// byte where the two answers differ, unless they are equal; it answers the cursor the route's page
// answered, "" for none.
func (replay *twoCutsReplay) page(r headRig, handler http.Handler, served *server, class, key, read, query, login, cursor string, page int) string {
	t := replay.t
	t.Helper()
	target := "/api/v1/issues/" + key + "/tree" + query
	if cursor != "" {
		target = treeCursorTarget(target, cursor)
	}
	request := func() *http.Request {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("X-Dispatch-User", login)
		request.Header.Set(displayTreesHeader, "1")
		return request
	}
	for attempt := 1; ; attempt++ {
		before := r.seq()
		answered := httptest.NewRecorder()
		handler.ServeHTTP(answered, request())
		routeStatus, route := answered.Code, answered.Body.Bytes()
		uncutStatus, uncut := replay.unmarked(r, key, func() (int, []byte) { return uncutTreeResponse(served, request(), key) })
		if r.seq() != before {
			if attempt == 5 {
				t.Fatalf("the store moved under each of %d pairs of reads of %s", attempt, read)
			}
			continue
		}
		if routeStatus >= http.StatusInternalServerError || uncutStatus >= http.StatusInternalServerError {
			t.Fatalf("the replay, after the %s write: %s, page %d (GET %s): the route answers status %d %s and the un-cut walk status %d %s",
				class, read, page, target, routeStatus, cut(route), uncutStatus, cut(uncut))
		}
		if routeStatus != uncutStatus || !bytes.Equal(route, uncut) {
			at := renderAtVersionFirstDifference(string(route), string(uncut))
			t.Errorf("the replay, after the %s write: %s, page %d (GET %s): the route answers status %d and the un-cut walk status %d, and the two answers differ "+
				"from byte %d: the route's %q, the un-cut walk's %q", class, read, page, target, routeStatus, uncutStatus, at, twoCutsAround(route, at), twoCutsAround(uncut, at))
		}
		var next struct {
			Cursor *string `json:"cursor"`
		}
		if routeStatus != http.StatusOK || json.Unmarshal(route, &next) != nil || next.Cursor == nil {
			return ""
		}
		return *next.Cursor
	}
}

// unmarked runs read with the mark of key taken away for its length (headRig.withoutMark, as the
// head projection's walkRead takes it), so the read walks the tuples, and puts the mark back as it
// stood; a key no mark names is read as it stands.
func (replay *twoCutsReplay) unmarked(r headRig, key string, read func() (int, []byte)) (int, []byte) {
	replay.t.Helper()
	return r.withoutMark(replay.t, key, false, read)
}

// treeCursorTarget is target, a GET of the tree route with its query or none, asking for the page
// cursor names.
func treeCursorTarget(target, cursor string) string {
	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}
	return target + separator + "cursor=" + url.QueryEscape(cursor)
}

// twoCutsAround is the bytes of answer from 80 before at to 200 after it, which a failure quotes.
func twoCutsAround(answer []byte, at int) string {
	return string(answer[max(0, at-80):min(len(answer), at+200)])
}
