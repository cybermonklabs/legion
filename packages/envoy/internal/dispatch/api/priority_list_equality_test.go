package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// mainListFilteredTerm keeps the merge base's query shape: the `advances` filter joins
// `advancing` and `advances_originating` to each issue, and the query reads rows straight
// from that scan. The optimized query tests each key's membership and reads the matched
// rows back from an array. The oracle uses the current label-fold map at $16 and $17,
// with the pinned login at $18, so both statements accept the same arguments and match
// labels the same way. The term is written out independently so a change to the production
// priority query cannot change both sides of the comparison.
func mainListFilteredTerm(pinned bool) string {
	pinnedJoin := ""
	if pinned {
		pinnedJoin = "\n\t\tjoin user_issue_state s on s.issue_key = i.key and s.login = $18 and s.pinned"
	}
	return `filtered as (
		select i.key, i.status, i.rank, i.created_at
		from issues i` + pinnedJoin + `
		left join advancing on advancing.key = i.key
		left join advances_originating originating on originating.key = i.key
		where ($1 = '' or i.project_key = $1)
		  and (cardinality($2::text[]) = 0 or i.status = any($2::text[]))
		  and ($3 = '' or i.parent_key = $3)
		  and ($4::timestamptz is null or i.updated_at >= $4)
		  and ($5::text[] = '{}' or (select array_agg(translate(label, $16::text, $17::text)) from unnest(i.labels) as label) @> $5)
		  and (not $6::boolean or i.closed_at is null)
		  and ((cardinality($7::smallint[]) = 0 and not $8::boolean)
		       or i.priority = any($7::smallint[])
		       or ($8::boolean and i.priority is null))
		  and ($9::timestamptz is null or i.closed_at >= $9)
		  and ($10::timestamptz is null or i.closed_at < $10)
		  and ((cardinality($11::text[]) = 0 and not $12::boolean and not $13::boolean)
		       or (not $15::boolean and (coalesce(advancing.named, false)
		           or ($12::boolean and advancing.key is null)
		           or ($13::boolean and advancing.key is not null)))
		       or ($15::boolean and (coalesce(originating.named, false)
		           or ($13::boolean and originating.key is not null))))
	)`
}

// mainListStatement answers the merge base's text of sent, one of the four listing statements:
// sent with its terms from `matched` up to the term after `filtered`, the page or the subjects,
// replaced by mainListFilteredTerm (explaintest.Spliced, which stops the test where the text does
// not change: at the merge base itself, which has no `matched` term, the comparison stops there).
func mainListStatement(t *testing.T, sent string) string {
	t.Helper()
	pinned := sent == listPinnedIssuesQuery || sent == listPinnedIssuesPageQuery
	closes := ", subjects as ("
	if sent == listIssuesPageQuery || sent == listPinnedIssuesPageQuery {
		closes = ", page as ("
	}
	return explaintest.Spliced(t, sent, explaintest.Part{
		Name: "the merge base's filtered term", Opens: "matched as (", Closes: closes, Text: mainListFilteredTerm(pinned),
	})
}

// TestAPriorityListAnswersAsTheMergeBaseAnswers checks that the optimized listing gives the
// same bytes as the merge base's query shape, with both queries using the current label matching.
// Over the corpus of production's counts (seedListCorpus), each request below is made of the
// server, and the listing statement it sends is run again with the same arguments twice: in the
// oracle's text on a connection that plans each run for its arguments, and in its own text on one
// generic plan. The answer the server wrote, and the generic plan's answer encoded as the server
// encodes it, must each be the bytes of the merge base's answer.
//
// The requests are the `advances` filter in every form the route takes (one id, two, none, any, a
// keyword beside an id, the id of a priority that is gone and one no priority carries) and every
// filter the existing listIssues tests drive, each alone and beside it, unpaged and on two pages,
// and a page past the end. A comparison of two empty answers proves nothing, so each request
// whose filter can match must answer an issue in the merge base's text, and the counts the corpus
// was built to must hold there: 598 issues under the largest priority, 7,677 under any and the
// rest under none.
func TestAPriorityListAnswersAsTheMergeBaseAnswers(t *testing.T) {
	t.Parallel()
	corpus := seedListCorpus(t)
	t.Log(corpus.String())
	trace := &speedTrace{}
	handler, _, _, _ := newRegisteredTestServer(t, testServerOptions{store: corpus.database, tracer: trace, identity: headerIdentity(corpus.database)})
	mergeBase := &server{deps: Deps{Store: planCacheStore(t, corpus.database, "force_custom_plan", 4)}}
	generic := &server{deps: Deps{Store: planCacheStore(t, corpus.database, "force_generic_plan", 4)}}

	largest, second, third := corpus.priorities[0], corpus.priorities[1], corpus.priorities[2]
	moment := func(place int) string {
		return url.QueryEscape(corpus.start.Add(time.Duration(place) * time.Minute).Format(time.RFC3339))
	}
	for _, filter := range []struct {
		query string
		// total is the count the merge base's answer must carry, 0 for any count above none, and
		// empty marks a filter that matches no issue.
		total int
		empty bool
	}{
		{query: ""},
		{query: "project=PRD02"},
		{query: "status=in_progress"},
		{query: "status=todo&status=backlog"},
		{query: "parent=" + corpus.parents[0]},
		{query: "parent=" + corpus.parents[1]},
		{query: "updated_since=" + moment(listCorpusIssues/2)},
		{query: "label=alpha"},
		{query: "label=alpha&label=beta"},
		{query: "open=true"},
		{query: "priority=1"},
		{query: "priority=none"},
		{query: "priority=0&priority=none"},
		{query: "closed_since=" + moment(listCorpusIssues/3) + "&closed_before=" + moment(2*listCorpusIssues/3)},
		{query: "advances=" + largest, total: listCorpusLargest},
		{query: "advances=" + largest + "&advances=" + second},
		{query: "advances=none", total: listCorpusIssues - listCorpusAdvancing},
		{query: "advances=any", total: listCorpusAdvancing},
		{query: "advances=none&advances=" + third},
		{query: "advances=any&advances=" + third, total: listCorpusAdvancing},
		{query: "advances=none&advances=any", total: listCorpusIssues},
		{query: "advances=" + corpus.gone, empty: true},
		{query: "advances=no-such-priority", empty: true},
		{query: "advances=" + largest + "&open=true&status=in_progress"},
		{query: "advances=none&project=PRD03"},
		{query: "advances=any&label=alpha&priority=2"},
		{query: "advances=" + largest + "&parent=" + corpus.parents[0]},
		{query: "advances=none&parent=" + corpus.parents[1]},
		{query: "advances=" + largest + "&updated_since=" + moment(listCorpusIssues/2)},
		{query: "advances=any&closed_since=" + moment(listCorpusIssues/3) + "&closed_before=" + moment(2*listCorpusIssues/3)},
		{query: "pinned=true"},
		{query: "pinned=true&advances=" + largest},
		{query: "pinned=true&advances=none"},
		{query: "moved=true&status=backlog&advances=" + largest},
		{query: "advances=" + largest + "&own=true"},
		{query: "advances=any&own=true"},
		{query: "advances=none&own=true", empty: true},
		{query: "advances=any&own=true&status=backlog&status=icebox&moved=true"},
	} {
		for _, paging := range []string{"", "limit=50", "limit=50&offset=100"} {
			if filter.query == "" && paging == "" {
				// Every issue resolved, three times over, proves nothing the paged reads do not.
				continue
			}
			target := "/api/v1/issues?" + strings.Trim(filter.query+"&"+paging, "&")
			want := compareWithMergeBase(t, handler, trace, mergeBase, generic, target)
			switch {
			case filter.empty && want.total != 0:
				t.Errorf("%s answers %d issues in the merge base's text, want none", target, want.total)
			case !filter.empty && want.total == 0:
				t.Errorf("%s answers no issue in the merge base's text, so its comparison proves nothing", target)
			case filter.total != 0 && want.total != filter.total:
				t.Errorf("%s answers %d issues in the merge base's text, want the corpus's %d", target, want.total, filter.total)
			}
		}
	}
	if want := compareWithMergeBase(t, handler, trace, mergeBase, generic,
		fmt.Sprintf("/api/v1/issues?advances=%s&limit=50&offset=%d", largest, listCorpusLargest+2)); want.total != listCorpusLargest || len(want.keys) != 0 {
		t.Errorf("a page past the end answers %d issues of %d in the merge base's text, want none of %d", len(want.keys), want.total, listCorpusLargest)
	}
}

// listAnswer is one listing answer: the bytes the route writes for it, its issues' keys in
// order, and the count of every issue its filters match.
type listAnswer struct {
	body  []byte
	keys  []string
	total int
}

// compareWithMergeBase makes one request of the server, runs the listing statement it sent in the
// merge base's text (on mergeBase's connections) and in its own text under the generic plan (on
// generic's), and holds the server's answer and the generic plan's to the merge base's, byte for
// byte. It answers the merge base's.
func compareWithMergeBase(t *testing.T, handler http.Handler, trace *speedTrace, mergeBase, generic *server, target string) listAnswer {
	t.Helper()
	response, listing := listingRequest(t, handler, trace, target)
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse %s: %v", target, err)
	}
	page, err := parseIssuePage(parsed.Query())
	if err != nil {
		t.Fatalf("read the page of %s: %v", target, err)
	}
	want := listStatementAnswer(t, mergeBase, mainListStatement(t, listing.sql), listing.arguments, page)
	served := listAnswer{body: response.Body.Bytes()}
	served.keys, served.total = listAnswerKeys(t, served.body, page != nil)
	holdToMergeBase(t, target, "the server's answer", served, want)
	holdToMergeBase(t, target, "the answer of its text under the generic plan", listStatementAnswer(t, generic, listing.sql, listing.arguments, page), want)
	return want
}

// listStatementAnswer runs one listing statement on s's connections, through the scan the route
// reads it with, and encodes the rows as the route writes them: the page it cut beside its count,
// or every row. The requests of this test send no sort, so a page names none (issueSortListing).
func listStatementAnswer(t *testing.T, s *server, statement string, arguments []any, page *issuePage) listAnswer {
	t.Helper()
	var (
		issues []model.IssueSummary
		total  int
		err    error
	)
	if page != nil {
		issues, total, err = s.scanIssuePage(context.Background(), statement, arguments)
	} else {
		issues, err = s.scanIssueSummaries(context.Background(), statement, arguments)
		total = len(issues)
	}
	if err != nil {
		t.Fatalf("run the listing statement: %v", err)
	}
	response := httptest.NewRecorder()
	if page != nil {
		WriteJSON(response, http.StatusOK, page.holding(issues, total, issueSortListing))
	} else {
		WriteJSON(response, http.StatusOK, issues)
	}
	answer := listAnswer{body: response.Body.Bytes(), total: total}
	for _, issue := range issues {
		answer.keys = append(answer.keys, issue.Key)
	}
	return answer
}

// listAnswerKeys reads the keys and the count out of an answer the route wrote.
func listAnswerKeys(t *testing.T, body []byte, paged bool) ([]string, int) {
	t.Helper()
	var issues []model.IssueSummary
	total := 0
	if paged {
		var page model.IssueSummaryPage
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("decode a page of the listing: %v", err)
		}
		issues, total = page.Issues, page.Total
	} else {
		if err := json.Unmarshal(body, &issues); err != nil {
			t.Fatalf("decode the listing: %v", err)
		}
		total = len(issues)
	}
	keys := []string{}
	for _, issue := range issues {
		keys = append(keys, issue.Key)
	}
	return keys, total
}

// holdToMergeBase fails the test when got is not want's bytes, naming the issues one answers and
// the other does not, and where the bytes first part.
func holdToMergeBase(t *testing.T, target, what string, got, want listAnswer) {
	t.Helper()
	if bytes.Equal(got.body, want.body) {
		return
	}
	missing, extra := []string{}, []string{}
	for _, key := range want.keys {
		if !slices.Contains(got.keys, key) {
			missing = append(missing, key)
		}
	}
	for _, key := range got.keys {
		if !slices.Contains(want.keys, key) {
			extra = append(extra, key)
		}
	}
	at := 0
	for at < min(len(got.body), len(want.body)) && got.body[at] == want.body[at] {
		at++
	}
	excerpt := func(body []byte) string { return string(body[max(at-80, 0):min(at+160, len(body))]) }
	t.Errorf("%s: %s is not the merge base's bytes: %d issues of %d against %d of %d; missing from it %v, not in the merge base's %v; the bytes part at %d:\n got  %s\n want %s",
		target, what, len(got.keys), got.total, len(want.keys), want.total, missing, extra, at, excerpt(got.body), excerpt(want.body))
}
