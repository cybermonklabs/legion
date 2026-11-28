package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestDispatchRLSReadsWritesAndSearch holds plan.md's trace rows of R1 and R3 for the API
// (INFRA-6168): one world (newRLSWorld) whose server runs every request on the request store, as
// dispatch_app under the caller's viewer, and the work before the viewer on the system store.
// In it HUSH and VEIL are private and owned by alice; HUSH lists the group crew, whose limited
// member is lena; VEIL and the public LIST list the limited member liam; una is a limited member
// no list names; bob is a full member no list names. Each row drives the real HTTP surface with
// real credentials: the sign-in (the test header identity), broker tokens, personal tokens and a
// results token. A row that writes is placed after the rows that read what it could change.
func TestDispatchRLSReadsWritesAndSearch(t *testing.T) {
	t.Parallel()
	w := newRLSWorld(t)
	open, hush := w.content[rlsOpen], w.content[rlsHush]
	bob := w.browser(rlsMember)
	hidden := w.hiddenMarks(rlsHush, rlsVeil)

	// R1: the single visibility rule (plan D8).
	t.Run("limited_member_through_group_reads_project", func(t *testing.T) {
		w.requireSight(t, "lena, a limited member of crew, which HUSH lists,", w.browser(rlsGrouped), rlsHush)
		if projects := listedProjects(t, w.browser(rlsGrouped)); !slices.Equal(projects, []string{rlsHush}) {
			t.Errorf("lena's projects are %v, want HUSH alone, which lists her group", projects)
		}
	})
	t.Run("limited_member_listed_directly_reads_project", func(t *testing.T) {
		w.requireSight(t, "liam, a limited member VEIL and LIST list,", w.browser(rlsListed), rlsVeil, rlsList)
		if projects := listedProjects(t, w.browser(rlsListed)); !slices.Equal(projects, []string{rlsList, rlsVeil}) {
			t.Errorf("liam's projects are %v, want LIST and VEIL, which list him", projects)
		}
	})
	t.Run("limited_member_not_listed_reads_no_public_project", func(t *testing.T) {
		if sees(t, w.browser(rlsListed), open.issue) {
			t.Errorf("liam, a limited member, reads %s of the public project OPEN, which lists him nowhere", open.issue)
		}
		if !sees(t, bob, open.issue) {
			t.Errorf("bob, a full member, does not read %s of the public project OPEN", open.issue)
		}
	})
	// The architect's row (ruling on departure 6): an open viewer passes a public project by the
	// project's own visibility, and that arm never passes a limited member's credential.
	t.Run("a_limited_member_does_not_see_a_public_project_its_list_does_not_name", func(t *testing.T) {
		w.requireSight(t, "una, a limited member no list names,", w.browser(rlsUnlisted))
		if projects := listedProjects(t, w.browser(rlsUnlisted)); len(projects) != 0 {
			t.Errorf("una's projects are %v, want none: a limited member sees a public project only where a list names it", projects)
		}
		w.requireSight(t, "una's broker token listing nothing", w.brokered(w.personToken(rlsUnlisted)))
	})
	t.Run("token_listing_none_sees_public_only", func(t *testing.T) {
		w.requireSight(t, "alice's broker token listing nothing", w.brokered(w.personToken(rlsOwner)), rlsOpen, rlsList)
	})
	t.Run("token_listing_one_private_sees_it", func(t *testing.T) {
		w.requireSight(t, "alice's broker token listing HUSH", w.brokered(w.personToken(rlsOwner, rlsHush)), rlsOpen, rlsHush, rlsList)
	})
	t.Run("token_listing_two_private_sees_both", func(t *testing.T) {
		w.requireSight(t, "alice's broker token listing HUSH and VEIL", w.brokered(w.personToken(rlsOwner, rlsHush, rlsVeil)), rlsOpen, rlsHush, rlsVeil, rlsList)
	})
	t.Run("limited_members_token_list_keeps_public_projects_its_person_sees", func(t *testing.T) {
		w.requireSight(t, "liam's broker token listing nothing", w.brokered(w.personToken(rlsListed)), rlsList)
		w.requireSight(t, "liam's broker token listing VEIL", w.brokered(w.personToken(rlsListed, rlsVeil)), rlsVeil, rlsList)
	})
	t.Run("personal_token_listing_private_p_sees_p", func(t *testing.T) {
		w.requireSight(t, "alice's personal token listing HUSH", w.personal(w.personalToken(t, rlsOwner, rlsHush)), rlsOpen, rlsHush, rlsList)
	})
	t.Run("the_same_personal_token_with_an_empty_list_does_not_see_p", func(t *testing.T) {
		token := w.personalToken(t, rlsOwner, rlsHush)
		if !sees(t, w.personal(token), hush.issue) {
			t.Fatalf("alice's personal token listing HUSH does not read %s", hush.issue)
		}
		w.setTokenList(t, token)
		w.requireSight(t, "the same personal token with its list emptied", w.personal(token), rlsOpen, rlsList)
	})
	t.Run("results_token_writes_only_its_project", func(t *testing.T) {
		answer := resultsOnlyToken(w.caller.jkt, rlsOpen)
		token := w.brokerToken(answer)
		result := func(project string) *httptest.ResponseRecorder {
			return w.presenting(t, token, http.MethodPost, "/api/v1/projects/"+project+"/results", passOf("k1"))
		}
		if other := result(rlsList); other.Code != http.StatusForbidden || !strings.Contains(other.Body.String(), codeProjectScope) {
			t.Errorf("the results token of OPEN writing LIST's results: status=%d body=%s, want 403 %s", other.Code, other.Body.String(), codeProjectScope)
		}
		answeredAsMissing(t, "the results token of OPEN writing a hidden project's results", result(rlsHush), result(rlsMissingProject), rlsHush, rlsMissingProject)
		if own := result(rlsOpen); strings.Contains(own.Body.String(), codeProjectScope) {
			t.Errorf("the results token of OPEN writing OPEN's results is refused %s: %s", codeProjectScope, own.Body.String())
		}
	})

	// R3: reads.
	t.Run("hidden_issue_artifact_comment_ask_message_project_read_answers_as_missing", func(t *testing.T) {
		for _, read := range []struct{ kind, hidden, missing, hiddenName, missingName string }{
			{"issue", "/api/v1/issues/" + hush.issue, "/api/v1/issues/" + rlsMissingIssue, hush.issue, rlsMissingIssue},
			{"artifact", "/api/v1/artifacts/" + hush.spec, "", hush.spec, ""},
			{"comment", "/api/v1/comments/" + hush.comment, "", hush.comment, ""},
			{"ask", "/api/v1/asks/" + hush.ask, "", hush.ask, ""},
			{"message", "/api/v1/messages/" + hush.message, "", hush.message, ""},
			{"project", "/api/v1/projects/" + rlsHush + "/artifacts", "/api/v1/projects/" + rlsMissingProject + "/artifacts", rlsHush, rlsMissingProject},
		} {
			missing, missingName := read.missing, read.missingName
			if missing == "" {
				missingName = missingUUID()
				missing = strings.Replace(read.hidden, read.hiddenName, missingName, 1)
			}
			answeredAsMissing(t, "bob's read of a hidden "+read.kind, bob(t, http.MethodGet, read.hidden, nil), bob(t, http.MethodGet, missing, nil), read.hiddenName, missingName)
		}
	})
	t.Run("lists_hold_no_hidden_row", func(t *testing.T) {
		for _, list := range []struct{ what, target, visible string }{
			{"the issues", "/api/v1/issues", `"key":"` + open.issue + `"`},
			{"the projects", "/api/v1/projects", `"key":"` + rlsOpen + `"`},
			{"the inbox", "/api/v1/inbox", open.ask},
			{"open asks", "/api/v1/asks/open?project=" + rlsOpen, open.ask},
			{"the priorities", "/api/v1/priorities", open.priority},
			// The sessions Dispatch holds a record of, of the two asked: the one that wrote only in
			// HUSH has no row, as an id with no record has none.
			{"known sessions", "/api/v1/agents/known?id=" + open.session + "&id=" + hush.session, open.session},
			{"the event log", "/api/v1/events/log?after=0", `"issue_key":"` + open.issue + `"`},
			{"alice's person page", "/api/v1/people/" + rlsOwner + "/nodes", open.comment},
			{"replies", "/api/v1/me/replies", open.reply},
		} {
			response := bob(t, http.MethodGet, list.target, nil)
			if response.Code != http.StatusOK {
				t.Errorf("bob's GET %s: status=%d body=%s", list.target, response.Code, cutBody(response.Body.String()))
				continue
			}
			requireNothingHidden(t, "bob's "+list.what, response.Body.String(), hidden)
			requireHolds(t, "bob's "+list.what, response.Body.String(), list.visible)
		}
		hiddenAsks := bob(t, http.MethodGet, "/api/v1/asks/open?project="+rlsHush, nil)
		missingAsks := bob(t, http.MethodGet, "/api/v1/asks/open?project="+rlsMissingProject, nil)
		hiddenAsks.Body = bytes.NewBufferString(strings.ReplaceAll(hiddenAsks.Body.String(), rlsHush, rlsMissingProject))
		if difference := allPublicDifference(hiddenAsks, missingAsks, []volatileMember{{"as_of", readTime}}); difference != "" {
			t.Errorf("bob's open asks of a hidden project answer otherwise than a missing project's: %s", difference)
		}
	})
	t.Run("counts_hold_no_hidden_row", func(t *testing.T) {
		var visible int
		if err := w.database.Pool.QueryRow(context.Background(), `select count(*) from issues where project_key = any($1)`,
			[]string{rlsOpen, rlsList}).Scan(&visible); err != nil {
			t.Fatalf("count the issues of the projects bob sees: %v", err)
		}
		page := bob(t, http.MethodGet, "/api/v1/issues?limit=200&offset=0", nil)
		if page.Code != http.StatusOK {
			t.Fatalf("bob's issue page: status=%d body=%s", page.Code, page.Body.String())
		}
		if total := decodeBody[model.IssueSummaryPage](t, page).Total; total != visible {
			t.Errorf("bob's issue list totals %d issues, want the %d of OPEN and LIST", total, visible)
		}
		type issueCounts struct {
			CommentCount      int `json:"comment_count"`
			ReferencedByCount int `json:"referenced_by_count"`
		}
		read := func(login string) issueCounts {
			response := w.asOK(t, login, http.MethodGet, "/api/v1/issues/"+open.issue, nil, http.StatusOK)
			return decodeBody[issueCounts](t, response)
		}
		bobs, alices := read(rlsMember), read(rlsOwner)
		if bobs.ReferencedByCount != alices.ReferencedByCount-1 {
			t.Errorf("%s counts %d backlinks for bob and %d for alice, want one fewer for bob: HUSH's remark cites it", open.issue, bobs.ReferencedByCount, alices.ReferencedByCount)
		}
		var comments int
		if err := w.database.Pool.QueryRow(context.Background(), `select count(*) from comments where issue_key = $1`, open.issue).Scan(&comments); err != nil {
			t.Fatalf("count %s's comments: %v", open.issue, err)
		}
		if bobs.CommentCount != comments {
			t.Errorf("%s counts %d comments for bob, want its %d", open.issue, bobs.CommentCount, comments)
		}
	})
	t.Run("keyword_search_holds_no_hidden_match", func(t *testing.T) {
		// The cut is taken first and the keyword index of past text filled after it, so the fill
		// has reached the cut (textversions.FillToHorizon).
		at := databaseNow(t, w.database.Pool).UTC()
		fillTextVersionsPast(t, w.database, asof.CutAt(at))
		cut := at.Format(time.RFC3339Nano)
		for _, search := range []struct{ what, target string }{
			{"the keyword search", "/api/v1/search?q=" + rlsCommonWord + "&limit=50"},
			{"the keyword search at a past time", "/api/v1/search/results?q=" + rlsCommonWord + "&limit=50&as_of=" + url.QueryEscape(cut)},
		} {
			response := bob(t, http.MethodGet, search.target, nil)
			if response.Code != http.StatusOK {
				t.Errorf("bob's %s: status=%d body=%s", search.what, response.Code, cutBody(response.Body.String()))
				continue
			}
			requireNothingHidden(t, "bob's "+search.what, response.Body.String(), hidden)
			kinds := map[string]bool{}
			for _, result := range decodeBody[model.SearchResponse](t, response).Results {
				kinds[result.Kind] = true
			}
			for _, kind := range []string{"issue", "comment", "ask", "message", "document"} {
				if !kinds[kind] {
					t.Errorf("bob's %s answers no %s of OPEN, so its %s arm proves nothing: %s", search.what, kind, kind, cutBody(response.Body.String()))
				}
			}
		}
	})
	t.Run("meaning_search_holds_no_hidden_match", func(t *testing.T) {
		target := "/api/v1/search?q=" + url.QueryEscape(rlsMeaningQuery) + "&limit=50"
		for _, reader := range []struct {
			login  string
			marks  []string
			holds  string
			hidden bool
		}{{rlsMember, hidden, open.issue, false}, {rlsOwner, nil, hush.issue, true}} {
			response := w.asOK(t, reader.login, http.MethodGet, target, nil, http.StatusOK)
			found := decodeBody[model.SearchResponse](t, response)
			if found.Degraded != "" {
				t.Fatalf("%s's meaning search answered degraded %q: the meaning leg did not run", reader.login, found.Degraded)
			}
			requireNothingHidden(t, reader.login+"'s meaning search", response.Body.String(), reader.marks)
			requireHolds(t, reader.login+"'s meaning search", response.Body.String(), reader.holds)
		}
	})
	t.Run("graph_edges_holds_no_hidden_edge", func(t *testing.T) {
		target := "/api/v1/references?to=" + url.QueryEscape("dispatch://"+open.issue) + "&since=0"
		requireNothingHidden(t, "bob's backlinks of "+open.issue, w.asOK(t, rlsMember, http.MethodGet, target, nil, http.StatusOK).Body.String(), hidden)
		requireHolds(t, "alice's backlinks of "+open.issue, w.asOK(t, rlsOwner, http.MethodGet, target, nil, http.StatusOK).Body.String(), hush.cite)
	})
	t.Run("references_hold_no_hidden_source", func(t *testing.T) {
		target := "/api/v1/issues/" + open.issue + "/references"
		requireNothingHidden(t, "bob's references of "+open.issue, w.asOK(t, rlsMember, http.MethodGet, target, nil, http.StatusOK).Body.String(), hidden)
		requireHolds(t, "alice's references of "+open.issue, w.asOK(t, rlsOwner, http.MethodGet, target, nil, http.StatusOK).Body.String(), hush.cite)
	})
	t.Run("graph_reads_hold_no_hidden_node", func(t *testing.T) {
		ref := func(key string) string { return url.QueryEscape("dispatch://" + key) }
		for _, read := range []struct{ what, hidden, missing string }{
			{"the reference targets", "/api/v1/reference-targets?ref=" + ref(hush.issue) + "&ref=" + ref(open.issue), "/api/v1/reference-targets?ref=" + ref(rlsMissingIssue) + "&ref=" + ref(open.issue)},
			{"the block links", "/api/v1/block-links?from=" + ref(hush.issue+"/spec") + "&to=" + ref(open.issue+"/spec"), "/api/v1/block-links?from=" + ref(rlsMissingIssue+"/spec") + "&to=" + ref(open.issue+"/spec")},
			{"the node's backlinks", "/api/v1/references?to=" + ref(hush.issue), "/api/v1/references?to=" + ref(rlsMissingIssue)},
		} {
			answeredAsMissing(t, "bob's read of "+read.what+" of a hidden node", bob(t, http.MethodGet, read.hidden, nil), bob(t, http.MethodGet, read.missing, nil), hush.issue, rlsMissingIssue)
		}
		missing := missingUUID()
		answeredAsMissing(t, "bob's read of a hidden document's references", bob(t, http.MethodGet, "/api/v1/artifacts/"+hush.spec+"/references", nil),
			bob(t, http.MethodGet, "/api/v1/artifacts/"+missing+"/references", nil), hush.spec, missing)
	})
	t.Run("related_and_duplicate_advice_names_no_hidden_issue", func(t *testing.T) {
		query := "?" + url.Values{"title": {"The " + hush.word + " " + rlsCommonWord + " issue"}, "text": {"The " + hush.word + " " + rlsCommonWord + " spec."}}.Encode()
		answeredAsMissing(t, "bob's related advice of a hidden project", bob(t, http.MethodGet, "/api/v1/projects/"+rlsHush+"/related"+query, nil),
			bob(t, http.MethodGet, "/api/v1/projects/"+rlsMissingProject+"/related"+query, nil), rlsHush, rlsMissingProject)
		answeredAsMissing(t, "bob's near-copy checks of a hidden project", bob(t, http.MethodGet, "/api/v1/near-copy-checks?project="+rlsHush, nil),
			bob(t, http.MethodGet, "/api/v1/near-copy-checks?project="+rlsMissingProject, nil), rlsHush, rlsMissingProject)
		related := bob(t, http.MethodGet, "/api/v1/projects/"+rlsOpen+"/related"+query, nil)
		if related.Code != http.StatusOK {
			t.Fatalf("bob's related advice of OPEN: status=%d body=%s", related.Code, related.Body.String())
		}
		if strings.Contains(related.Body.String(), rlsHush+"-") {
			t.Errorf("bob's related advice of OPEN names an issue of HUSH: %s", cutBody(related.Body.String()))
		}
	})
	t.Run("legion_board_holds_no_hidden_project", func(t *testing.T) {
		nameLegionDaemon(t, w.handler, rlsHush, "http://legion-hush.internal:13370")
		nameLegionDaemon(t, w.handler, rlsOpen, "http://legion-open.internal:13370")
		answeredAsMissing(t, "bob's board of a hidden project", bob(t, http.MethodGet, "/api/v1/projects/"+rlsHush+"/legion/board", nil),
			bob(t, http.MethodGet, "/api/v1/projects/"+rlsMissingProject+"/legion/board", nil), rlsHush, rlsMissingProject)
		board := bob(t, http.MethodGet, "/api/v1/projects/"+rlsOpen+"/legion/board", nil)
		if board.Code != http.StatusOK {
			t.Fatalf("bob's board of OPEN: status=%d body=%s", board.Code, board.Body.String())
		}
		requireNothingHidden(t, "bob's board of OPEN", board.Body.String(), hidden)
	})
	t.Run("viewer_resolved_once_per_request", func(t *testing.T) {
		recorder := &storetest.StatementRecorder{}
		traced := newRLSServer(t, w.database, rlsServerConfig{system: storetest.TracedStore(t, w.database, recorder)})
		resolutions := func() int {
			count := 0
			for _, statement := range recorder.Statements() {
				if statement == access.VisibleProjectsSQL() {
					count++
				}
			}
			return count
		}
		for _, request := range []struct {
			method, target string
			body           any
			status         int
		}{
			{http.MethodGet, "/api/v1/issues/" + open.issue, nil, http.StatusOK},
			{http.MethodPost, "/api/v1/issues/" + open.issue + "/comments", map[string]any{"body": "One more remark."}, http.StatusCreated},
		} {
			before := resolutions()
			traced.asOK(t, rlsMember, request.method, request.target, request.body, request.status)
			if got := resolutions() - before; got != 1 {
				t.Errorf("%s %s resolved bob's viewer %d times on the system store (access.Resolve), want once", request.method, request.target, got)
			}
		}
	})
	t.Run("a_request_context_carries_no_system_work_mark", func(t *testing.T) {
		// Each wrapper a request's context is made by: the route table's (routeContext), the
		// websockets' (trackTransactions) and the live socket's whole chain (liveSocketHandler).
		// A request whose base context carries the system-work mark reaches a handler whose
		// context carries none, so a statement it sends on the request store with no viewer is
		// refused (store.ErrNoViewer) rather than run on the system pool as the owner.
		for _, wrapper := range []struct {
			what string
			wrap func(http.HandlerFunc) http.Handler
		}{
			{"routeContext", func(probe http.HandlerFunc) http.Handler { return routeContext(probe) }},
			{"trackTransactions", func(probe http.HandlerFunc) http.Handler { return trackTransactions(probe) }},
			{"liveSocketHandler", func(probe http.HandlerFunc) http.Handler { return w.served.liveSocketHandler(probe) }},
		} {
			var err error
			ran := false
			probe := func(rw http.ResponseWriter, r *http.Request) {
				ran = true
				_, err = w.deps.Store.Pool.Exec(r.Context(), `select 1`)
				rw.WriteHeader(http.StatusNoContent)
			}
			request := withPerson(httptest.NewRequestWithContext(store.WithSystemWork(t.Context()), http.MethodGet, liveSocketPattern, nil), rlsMember)
			wrapper.wrap(probe).ServeHTTP(httptest.NewRecorder(), request)
			if !ran {
				t.Errorf("%s did not run the probe", wrapper.what)
				continue
			}
			if !errors.Is(err, store.ErrNoViewer) {
				t.Errorf("a statement with no viewer behind %s, on a request whose base context is marked as system work, answered %v, want store.ErrNoViewer", wrapper.what, err)
			}
		}
	})

	// R3: writes.
	t.Run("hidden_update_answers_as_missing", func(t *testing.T) {
		missing := missingUUID()
		answeredAsMissing(t, "bob's retitle of a hidden issue", bob(t, http.MethodPatch, "/api/v1/issues/"+hush.issue, map[string]any{"title": "Bob's retitle"}),
			bob(t, http.MethodPatch, "/api/v1/issues/"+rlsMissingIssue, map[string]any{"title": "Bob's retitle"}), hush.issue, rlsMissingIssue)
		answeredAsMissing(t, "bob's edit of his own comment on a hidden issue", bob(t, http.MethodPatch, "/api/v1/comments/"+hush.bobComment, map[string]any{"body": "Bob's edit."}),
			bob(t, http.MethodPatch, "/api/v1/comments/"+missing, map[string]any{"body": "Bob's edit."}), hush.bobComment, missing)
		var title, body string
		if err := w.database.Pool.QueryRow(context.Background(), `select (select title from issues where key = $1), (select body from comments where id = $2::uuid)`,
			hush.issue, hush.bobComment).Scan(&title, &body); err != nil {
			t.Fatalf("read the hidden issue and comment: %v", err)
		}
		if strings.Contains(title, "Bob's") || strings.Contains(body, "Bob's edit") {
			t.Errorf("bob's refused updates wrote the hidden rows: title %q, comment %q", title, body)
		}
	})
	t.Run("hidden_delete_answers_as_missing", func(t *testing.T) {
		missing := missingUUID()
		answeredAsMissing(t, "bob's delete of his own comment on a hidden issue", bob(t, http.MethodDelete, "/api/v1/comments/"+hush.bobDoomed, nil),
			bob(t, http.MethodDelete, "/api/v1/comments/"+missing, nil), hush.bobDoomed, missing)
		var deleted bool
		if err := w.database.Pool.QueryRow(context.Background(), `select deleted_at is not null from comments where id = $1::uuid`, hush.bobDoomed).Scan(&deleted); err != nil {
			t.Fatalf("read the hidden comment: %v", err)
		}
		if deleted {
			t.Errorf("bob's refused delete deleted the hidden comment %s", hush.bobDoomed)
		}
	})
	t.Run("insert_under_a_hidden_parent_answers_as_missing", func(t *testing.T) {
		counts := func() string {
			var held string
			if err := w.database.Pool.QueryRow(context.Background(), `
				select (select count(*) from comments where issue_key = $1) || ' comments, ' || (select count(*) from issues where project_key = $2) || ' issues'
			`, hush.issue, rlsHush).Scan(&held); err != nil {
				t.Fatalf("count the hidden project's rows: %v", err)
			}
			return held
		}
		before := counts()
		answeredAsMissing(t, "bob's comment on a hidden issue", bob(t, http.MethodPost, "/api/v1/issues/"+hush.issue+"/comments", map[string]any{"body": "Bob's remark."}),
			bob(t, http.MethodPost, "/api/v1/issues/"+rlsMissingIssue+"/comments", map[string]any{"body": "Bob's remark."}), hush.issue, rlsMissingIssue)
		answeredAsMissing(t, "bob's issue filed into a hidden project",
			bob(t, http.MethodPost, "/api/v1/issues", map[string]any{"project": rlsHush, "title": "Bob's issue", "force": true}),
			bob(t, http.MethodPost, "/api/v1/issues", map[string]any{"project": rlsMissingProject, "title": "Bob's issue", "force": true}), rlsHush, rlsMissingProject)
		if after := counts(); after != before {
			t.Errorf("bob's refused inserts changed the hidden project from %s to %s", before, after)
		}
	})
	t.Run("an_update_or_upsert_moving_a_row_into_a_hidden_project_answers_as_missing", func(t *testing.T) {
		// Bob wrote his state of HUSH's session's conversation while HUSH was public. The session
		// wrote only in HUSH, so bob reads it no more, and his upsert of that row meets its
		// existing row under the policy's USING expression: PostgreSQL answers 42501 "new row
		// violates row-level security policy (USING expression)", which is the answer of a row
		// that is not there (writeHandlerError, plan D9), never a 500 and never a write.
		var before time.Time
		read := func() time.Time {
			var cleared time.Time
			if err := w.database.Pool.QueryRow(context.Background(), `select cleared_before from user_agent_state where login = $1 and session_id = $2`,
				rlsMember, hush.session).Scan(&cleared); err != nil {
				t.Fatalf("read bob's state of %s: %v", hush.session, err)
			}
			return cleared
		}
		before = read()
		response := bob(t, http.MethodPut, "/api/v1/me/agents/"+hush.session+"/state", map[string]any{"cleared_before": cutAtNow(t, w.database)})
		missing := httptest.NewRecorder()
		w.served.writeHandlerError(missing, pgx.ErrNoRows)
		if response.Code != missing.Code || response.Body.String() != missing.Body.String() {
			t.Errorf("bob's upsert of his state of a hidden session answered %d %s, want the missing row's %d %s",
				response.Code, response.Body.String(), missing.Code, missing.Body.String())
		}
		if after := read(); !after.Equal(before) {
			t.Errorf("bob's refused upsert moved his state of %s from %s to %s", hush.session, before, after)
		}
	})
	t.Run("scoped_broker_token_gets_project_scope_for_hidden_as_for_missing", func(t *testing.T) {
		answer := liveNamedToken(w.caller.jkt)
		answer.Projects = []string{rlsOpen}
		token := w.brokerToken(answer)
		comment := func(issue string) *httptest.ResponseRecorder {
			return w.presenting(t, token, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{"body": "A remark."})
		}
		hidden, missing := comment(hush.issue), comment(rlsMissingIssue)
		if hidden.Code != http.StatusForbidden || !strings.Contains(hidden.Body.String(), codeProjectScope) {
			t.Errorf("the token naming OPEN commenting on %s: status=%d body=%s, want 403 %s", hush.issue, hidden.Code, hidden.Body.String(), codeProjectScope)
		}
		answeredAsMissing(t, "the token naming OPEN commenting on a hidden issue", hidden, missing, hush.issue, rlsMissingIssue)
	})
	t.Run("a_hidden_id_prefix_answers_as_missing_not_ambiguous", func(t *testing.T) {
		// A comment of HUSH whose id starts as OPEN's remark's does: the short id both carry
		// names OPEN's remark alone for bob, who does not see HUSH, and is ambiguous for alice.
		shared := open.comment[:sharedCommentLength]
		var twin string
		if err := w.database.Pool.QueryRow(context.Background(), `select $1 || substr(gen_random_uuid()::text, $2)`, shared, sharedCommentLength+1).Scan(&twin); err != nil {
			t.Fatalf("draw the twin's id: %v", err)
		}
		if _, err := w.database.Pool.Exec(context.Background(), `
			insert into comments (id, issue_key, author, body) values ($1::uuid, $2, '{"kind":"user","id":"alice"}', $3)
		`, twin, hush.issue, "A "+hush.word+" "+rlsCommonWord+" twin."); err != nil {
			t.Fatalf("write HUSH's twin comment: %v", err)
		}
		read := bob(t, http.MethodGet, "/api/v1/comments/"+shared, nil)
		if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), open.comment) {
			t.Errorf("bob's read of the short id %s: status=%d body=%s, want OPEN's remark %s", shared, read.Code, cutBody(read.Body.String()), open.comment)
		}
		if both := w.as(t, rlsOwner, http.MethodGet, "/api/v1/comments/"+shared, nil); both.Code != http.StatusConflict {
			t.Errorf("alice's read of the short id %s both comments carry: status=%d body=%s, want 409", shared, both.Code, cutBody(both.Body.String()))
		}
		own := hush.comment[:sharedCommentLength]
		var nobody string
		for _, candidate := range []string{"ffffffff-ffff", "eeeeeeee-eeee", "dddddddd-dddd"} {
			var held bool
			if err := w.database.Pool.QueryRow(context.Background(), `select exists (select 1 from comments where id::text like $1 || '%')`, candidate).Scan(&held); err != nil {
				t.Fatalf("read whether a comment carries %s: %v", candidate, err)
			}
			if !held {
				nobody = candidate
				break
			}
		}
		answeredAsMissing(t, "bob's read of a short id only HUSH's remark carries", bob(t, http.MethodGet, "/api/v1/comments/"+own, nil),
			bob(t, http.MethodGet, "/api/v1/comments/"+nobody, nil), own, nobody)
	})
	t.Run("delivery_reads_hold_no_hidden_pull_request_or_repository_run", func(t *testing.T) {
		// The deploy repository is HUSH's alone: only an issue of HUSH resolves to it, so its runs
		// and its pull requests are rows of a repository bob does not see (the repository rule).
		w.asOK(t, rlsOwner, http.MethodPut, "/api/v1/projects/"+rlsHush+"/default-repository", map[string]any{"repo": "acme/widgets"}, http.StatusOK)
		if _, err := delivery.PutSettings(t.Context(), w.database.Pool, pipelineSettings(), model.Actor{Kind: "system", ID: "test"}); err != nil {
			t.Fatalf("seed the delivery settings: %v", err)
		}
		now := time.Now().UTC().Truncate(time.Second)
		const run = 9401
		at := now.Add(-10 * time.Hour)
		seedPipelineRun(t, w.database.Pool, delivery.DeliveryRun{
			RunID: run, Kind: delivery.DeliveryRunKindDeploy, StartedAt: at, CompletedAt: new(at.Add(10 * time.Minute)),
			Conclusion: new(delivery.DeliveryRunConclusionSuccess), HeadBranch: new("main"), Event: new("push"),
		}, []pipelineFixtureJob{{name: "widgets-release / widgets-release", conclusion: "success", start: at, end: at.Add(5 * time.Minute)}})
		merged, issue := at.Add(-time.Hour), hush.issue
		if err := delivery.UpsertPullRequest(t.Context(), w.database.Pool, delivery.DeliveryPullRequest{
			Repo: "acme/widgets", Number: 77, Title: "The " + hush.word + " " + rlsCommonWord + " pull request", Author: "octocat",
			URL: "https://github.com/acme/widgets/pull/77", CreatedAt: &merged, MergedAt: &merged, IssueKey: &issue,
		}); err != nil {
			t.Fatalf("seed the hidden pull request: %v", err)
		}
		window := url.Values{"from": {at.Add(-time.Hour).Format(time.RFC3339)}, "to": {at.Add(time.Hour).Format(time.RFC3339)}}.Encode()
		marks := append(slices.Clone(hidden), "/actions/runs/9401", "/pull/77")
		for _, read := range []string{"/api/v1/delivery/timeline?" + window, "/api/v1/delivery/measures?" + window, "/api/v1/delivery/pipeline?at=" + url.QueryEscape(now.Format(time.RFC3339))} {
			response := bob(t, http.MethodGet, read, nil)
			if response.Code != http.StatusOK {
				t.Errorf("bob's GET %s: status=%d body=%s", read, response.Code, cutBody(response.Body.String()))
				continue
			}
			requireNothingHidden(t, "bob's "+read, response.Body.String(), marks)
		}
		w.asOK(t, rlsOwner, http.MethodGet, "/api/v1/delivery/runs/9401", nil, http.StatusOK)
		answeredAsMissing(t, "bob's read of a run of a hidden repository", bob(t, http.MethodGet, "/api/v1/delivery/runs/9401", nil),
			bob(t, http.MethodGet, "/api/v1/delivery/runs/9499", nil), "9401", "9499")
	})
	t.Run("a_cycle_through_a_hidden_issue_is_still_refused", func(t *testing.T) {
		file := func(project, title string) string {
			return createdIssue(t, "file "+title, w.as(t, rlsOwner, http.MethodPost, "/api/v1/issues", map[string]any{"project": project, "title": title, "force": true})).Key
		}
		head, tail := file(rlsOpen, "The head of a wait"), file(rlsOpen, "The tail of a wait")
		middle := file(rlsHush, "The hidden middle of a wait")
		w.asOK(t, rlsOwner, http.MethodPatch, "/api/v1/issues/"+head, map[string]any{"blocked_by": []string{middle}}, http.StatusOK)
		w.asOK(t, rlsOwner, http.MethodPatch, "/api/v1/issues/"+middle, map[string]any{"blocked_by": []string{tail}}, http.StatusOK)
		closing := bob(t, http.MethodPatch, "/api/v1/issues/"+tail, map[string]any{"blocked_by": []string{head}})
		if code, _ := refusalOf(t, closing); closing.Code != http.StatusConflict || code != "DEPENDENCY_CYCLE" {
			t.Fatalf("bob's link closing a ring through %s: status=%d body=%s, want 409 DEPENDENCY_CYCLE", middle, closing.Code, closing.Body.String())
		}
		if strings.Contains(closing.Body.String(), middle) {
			t.Errorf("the refusal names the hidden issue %s: %s", middle, closing.Body.String())
		}
		var links int
		if err := w.database.Pool.QueryRow(context.Background(), `select count(*) from issue_links where issue_key = $1`, tail).Scan(&links); err != nil {
			t.Fatalf("count %s's links: %v", tail, err)
		}
		if links != 0 {
			t.Errorf("the refused link left %d links of %s, want none", links, tail)
		}
	})
	t.Run("architecture_sync_of_a_hidden_project_answers_as_missing", func(t *testing.T) {
		// HUSH and OPEN each read an architecture source through a GitHub App at a fake GitHub,
		// as architecture_source_test.go's servers do, so a sync of either has a source to read:
		// bob's sync of OPEN is taken, and his sync of HUSH answers as a project nobody holds, on
		// each of the three sync routes, before the importer, which runs as the owner, reads it.
		fake := twoSourceFake()
		options := architectureAppOptions(t, fake)
		server := newRLSServer(t, w.database, rlsServerConfig{app: options.app, githubAPIBase: options.githubAPIBase})
		sources := map[string]string{rlsHush: "legion/arch", rlsOpen: "acme/infra"}
		for _, project := range []string{rlsHush, rlsOpen} {
			server.asOK(t, rlsOwner, http.MethodPut, sourcePath(project, sources[project]), map[string]any{"branch": "main"}, http.StatusOK)
		}
		for _, route := range []string{"/architecture-source/sync", "/architecture-sources/sync", "/architecture-sources/" + sources[rlsHush] + "/sync"} {
			if synced := server.as(t, rlsMember, http.MethodPost, "/api/v1/projects/"+rlsOpen+strings.Replace(route, sources[rlsHush], sources[rlsOpen], 1), nil); synced.Code != http.StatusOK {
				t.Errorf("bob's sync %s of OPEN, which reads a source: status=%d body=%s, want 200", route, synced.Code, cutBody(synced.Body.String()))
			}
			answeredAsMissing(t, "bob's sync "+route+" of a hidden project", server.as(t, rlsMember, http.MethodPost, "/api/v1/projects/"+rlsHush+route, nil),
				server.as(t, rlsMember, http.MethodPost, "/api/v1/projects/"+rlsMissingProject+route, nil), rlsHush, rlsMissingProject)
		}
	})
	t.Run("a_document_cited_only_from_a_hidden_project_is_still_refused_removal", func(t *testing.T) {
		cited := emptyDocument(t, w.handler, open.issue, "Cited from a hidden project")
		uncited := emptyDocument(t, w.handler, open.issue, "Cited from nowhere")
		createThreadComment(t, w.handler, hush.issue, map[string]any{
			"body": "The " + hush.word + " " + rlsCommonWord + " cites dispatch://" + open.issue + "/artifact/" + cited.Slug + ".",
		}, rlsOwner)
		refused := bob(t, http.MethodDelete, "/api/v1/artifacts/"+cited.ID, nil)
		if code, _ := refusalOf(t, refused); refused.Code != http.StatusConflict || code != "ARTIFACT_NOT_UNTOUCHED" {
			t.Errorf("bob's removal of a document HUSH's remark cites: status=%d body=%s, want 409 ARTIFACT_NOT_UNTOUCHED", refused.Code, refused.Body.String())
		}
		requireNothingHidden(t, "the refusal", refused.Body.String(), append(slices.Clone(hidden), rlsHush+"-"))
		if removed := bob(t, http.MethodDelete, "/api/v1/artifacts/"+uncited.ID, nil); removed.Code != http.StatusNoContent {
			t.Errorf("bob's removal of a document nothing cites: status=%d body=%s, want 204", removed.Code, removed.Body.String())
		}
		var still bool
		if err := w.database.Pool.QueryRow(context.Background(), `select exists (select 1 from artifacts where id = $1::uuid)`, cited.ID).Scan(&still); err != nil {
			t.Fatalf("read the cited document: %v", err)
		}
		if !still {
			t.Errorf("the cited document %s is gone", cited.ID)
		}
	})
	t.Run("a_missing_grant_is_a_500_not_a_404", func(t *testing.T) {
		// A database of its own, since the grant this row takes away would break every row after
		// it: dispatch_app may not insert a comment there, which is a 42501 that is no row
		// security's, so the write answers 500 INTERNAL and not the 404 of a row that is not there.
		database := storetest.Open(t)
		seedPeople(t, database, rlsOwner)
		server := newRLSServer(t, database, rlsServerConfig{})
		server.asOK(t, rlsOwner, http.MethodPost, "/api/v1/projects", map[string]any{"key": "GRANT", "name": "Grant"}, http.StatusCreated)
		issue := createdIssue(t, "file the grant's issue", server.as(t, rlsOwner, http.MethodPost, "/api/v1/issues", map[string]any{"project": "GRANT", "title": "A remark nobody may make", "force": true}))
		if _, err := database.Pool.Exec(context.Background(), `revoke insert on comments from dispatch_app`); err != nil {
			t.Fatalf("take dispatch_app's insert on comments away: %v", err)
		}
		response := server.as(t, rlsOwner, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "A remark."})
		if code, _ := refusalOf(t, response); response.Code != http.StatusInternalServerError || code != codeInternal {
			t.Errorf("a comment dispatch_app may not insert: status=%d body=%s, want 500 %s", response.Code, response.Body.String(), codeInternal)
		}
	})

	// The owner-run rules of plan D4 that no route reaches on its own (dispatch_node_visible,
	// dispatch_tuple_end_visible, dispatch_broadcast_visible, dispatch_workflow_node_visible and
	// dispatch_message_visible), each read through the request store under a viewer while HUSH and
	// VEIL are private. alice sees every project; bob, an open full member, sees OPEN and LIST.
	ctx := context.Background()
	aliceSees := model.Viewer{Person: rlsOwner, Open: true, Sees: []string{rlsHush, rlsList, rlsOpen, rlsVeil}, AnyPrivate: true}
	bobSees := model.Viewer{Person: rlsMember, Open: true, Sees: []string{rlsList, rlsOpen}, AnyPrivate: true}
	countAs := func(t *testing.T, viewer model.Viewer, sql string, args ...any) int {
		t.Helper()
		var count int
		if err := w.deps.Store.Pool.QueryRow(store.WithViewer(ctx, viewer), sql, args...).Scan(&count); err != nil {
			t.Fatalf("count under %s's viewer: %v", viewer.Person, err)
		}
		return count
	}
	seed := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := w.database.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", strings.Join(strings.Fields(sql), " "), err)
		}
	}
	seedSessions := func(t *testing.T) {
		t.Helper()
		seed(t, `insert into session_projects (session_id, project_key, first_at) values ($1, $2, now()), ($3, $4, now()) on conflict do nothing`,
			open.session, rlsOpen, hush.session, rlsHush)
	}

	t.Run("person_session_and_role_nodes_stay_visible_to_the_viewers_their_clauses_admit_while_a_project_is_private", func(t *testing.T) {
		seedSessions(t)
		seed(t, `insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event, project_key) values
			('issue', $1, 'issue', 'project', 'live', now(), '{}', 0, $2),
			('person', 'rls-dora', 'person', 'project', 'live', now(), '{}', 0, null),
			('session', $3, 'session', 'project', 'live', now(), '{}', 0, null),
			('session', $4, 'session', 'project', 'live', now(), '{}', 0, null),
			('role', 'rls-role-open', 'role', 'project', 'live', now(), '{}', 0, null),
			('role', 'rls-role-hush', 'role', 'project', 'live', now(), '{}', 0, null)
			on conflict do nothing`, open.issue, rlsOpen, open.session, hush.session)
		seed(t, `insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event) values
			('role', 'rls-role-open', 'held_by', 'session', $2, now(), '{}', 0),
			('role', 'rls-role-hush', 'held_by', 'session', $3, now(), '{}', 0),
			('issue', $1, 'authored_by', 'person', 'rls-dora', now(), '{}', 0),
			('issue', $1, 'delivered_to', 'session', $3, now(), '{}', 0)`, open.issue, open.session, hush.session)
		seenNodes := `select count(*) from graph_nodes where (kind, id) in (('person', 'rls-dora'), ('session', $1), ('role', 'rls-role-open'))`
		hiddenNodes := `select count(*) from graph_nodes where (kind, id) in (('session', $1), ('role', 'rls-role-hush'))`
		seenTuples := `select count(*) from graph_tuples where from_id = 'rls-role-open' or (from_id = $1 and to_id = 'rls-dora')`
		hiddenTuples := `select count(*) from graph_tuples where from_id = 'rls-role-hush' or (from_id = $1 and kind = 'delivered_to' and to_id = $2)`
		for _, read := range []struct {
			viewer                                    model.Viewer
			nodes, hiddenNodes, tuples, hiddenTuples int
		}{{bobSees, 3, 0, 2, 0}, {aliceSees, 3, 2, 2, 2}} {
			got := []int{
				countAs(t, read.viewer, seenNodes, open.session), countAs(t, read.viewer, hiddenNodes, hush.session),
				countAs(t, read.viewer, seenTuples, open.issue), countAs(t, read.viewer, hiddenTuples, open.issue, hush.session),
			}
			if want := []int{read.nodes, read.hiddenNodes, read.tuples, read.hiddenTuples}; !slices.Equal(got, want) {
				t.Errorf("%s reads [the person, OPEN's session and its role; HUSH's session and the role only it holds; the tuples ending at the first; at the second] %v times, want %v",
					read.viewer.Person, got, want)
			}
		}
	})

	t.Run("with_no_private_project_the_node_rule_function_is_called_for_no_row", func(t *testing.T) {
		// The node rules are replaced, in a transaction rolled back at the end, by functions that
		// refuse every call, and the graph is read as dispatch_app with viewer_private false: the
		// fast path answers every row before either is called. The same read with viewer_private
		// true calls them, which shows the replacement is the function the policies call.
		tx, err := w.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `
			create or replace function public.dispatch_node_visible(node_kind text, node_id text, node_project text, node_parties text[]) returns boolean
			language plpgsql as $$ begin raise exception 'dispatch_node_visible was called'; end $$;
			create or replace function public.dispatch_tuple_end_visible(end_kind text, end_id text) returns boolean
			language plpgsql as $$ begin raise exception 'dispatch_tuple_end_visible was called'; end $$`); err != nil {
			t.Fatalf("replace the node rules with refusing functions: %v", err)
		}
		read := func(private string) (nodes, tuples int, err error) {
			nested, err := tx.Begin(ctx)
			if err != nil {
				return 0, 0, err
			}
			defer func() { _ = nested.Rollback(ctx) }()
			if _, err := nested.Exec(ctx, `select set_config('role', 'dispatch_app', true), set_config('dispatch.viewer_sees', '', true),
				set_config('dispatch.viewer_person', $1, true), set_config('dispatch.viewer_open', 'true', true),
				set_config('dispatch.viewer_session', '', true), set_config('dispatch.viewer_private', $2, true)`, rlsMember, private); err != nil {
				return 0, 0, err
			}
			err = nested.QueryRow(ctx, `select (select count(*) from graph_nodes), (select count(*) from graph_tuples)`).Scan(&nodes, &tuples)
			return nodes, tuples, err
		}
		nodes, tuples, err := read("false")
		if err != nil {
			t.Fatalf("with no private project the graph read as dispatch_app called a node rule: %v", err)
		}
		if nodes == 0 || tuples == 0 {
			t.Fatalf("the graph read as dispatch_app holds %d nodes and %d tuples, so the read proves nothing", nodes, tuples)
		}
		if _, _, err := read("true"); err == nil || !strings.Contains(err.Error(), "was called") {
			t.Errorf("with a private project the graph read answered %v, want the refusing node rule's error: the replacement is not what the policies call", err)
		}
	})

	t.Run("a_viewer_whose_broadcast_clause_refuses_reads_no_broadcast", func(t *testing.T) {
		seedSessions(t)
		seed(t, `insert into broadcasts (id, author, body, delivery, created_at) values
			('00000000-0000-4000-8000-00000000b001', '{"kind":"user","id":"alice"}', 'to both', 'btw', now()),
			('00000000-0000-4000-8000-00000000b002', '{"kind":"user","id":"alice"}', 'to OPEN', 'btw', now())`)
		seed(t, `insert into messages (id, author, body, target, broadcast_id, broadcast_position, created_at) values
			('00000000-0000-4000-8000-00000000b011', '{"kind":"user","id":"alice"}', 'to both', 'session:' || $1, '00000000-0000-4000-8000-00000000b001', 0, now()),
			('00000000-0000-4000-8000-00000000b012', '{"kind":"user","id":"alice"}', 'to both', 'session:' || $2, '00000000-0000-4000-8000-00000000b001', 1, now()),
			('00000000-0000-4000-8000-00000000b021', '{"kind":"user","id":"alice"}', 'to OPEN', 'session:' || $1, '00000000-0000-4000-8000-00000000b002', 0, now())`,
			open.session, hush.session)
		const both = `select count(*) from broadcasts where id = '00000000-0000-4000-8000-00000000b001'`
		const toOpen = `select count(*) from broadcasts where id = '00000000-0000-4000-8000-00000000b002'`
		if refused, read := countAs(t, bobSees, both), countAs(t, bobSees, toOpen); refused != 0 || read != 1 {
			t.Errorf("bob, who does not see HUSH's session a copy went to, reads that broadcast %d times (want 0) and the one to OPEN's alone %d (want 1)", refused, read)
		}
		if read := countAs(t, aliceSees, both); read != 1 {
			t.Errorf("alice, who sent it and sees every session it went to, reads the broadcast %d times, want 1", read)
		}
	})

	t.Run("a_workflow_node_insert_and_its_update_pass_in_one_transaction_and_a_viewer_who_cannot_see_its_issue_reads_no_node", func(t *testing.T) {
		viewed := store.WithViewer(ctx, aliceSees)
		tx, err := w.deps.Store.Pool.Begin(viewed)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		const node = `00000000-0000-4000-8000-00000000e001`
		if _, err := tx.Exec(viewed, `insert into workflow_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
			values ('step', $1, 'step', 'project', 'live', '{}', now(), '{"kind":"user","id":"alice"}', 0)`, node); err != nil {
			t.Fatalf("alice's insert of a step that names no issue yet: %v", err)
		}
		var held []byte
		if err := tx.QueryRow(viewed, `select attributes from workflow_nodes where kind = 'step' and id = $1 for update`, node).Scan(&held); err != nil {
			t.Fatalf("alice's read of the step she just inserted: %v", err)
		}
		if tag, err := tx.Exec(viewed, `update workflow_nodes set attributes = attributes || jsonb_build_object('issue', $2::text) where kind = 'step' and id = $1`, node, hush.issue); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("alice's update stating the step's issue %s: %v, %d rows", hush.issue, err, tag.RowsAffected())
		}
		if err := tx.Commit(viewed); err != nil {
			t.Fatal(err)
		}
		const read = `select count(*) from workflow_nodes where id = '00000000-0000-4000-8000-00000000e001'`
		if hidden, seen := countAs(t, bobSees, read), countAs(t, aliceSees, read); hidden != 0 || seen != 1 {
			t.Errorf("the step of %s reads %d times for bob (want 0) and %d for alice (want 1)", hush.issue, hidden, seen)
		}
	})

	t.Run("a_viewer_neither_sender_nor_recipient_reads_neither_the_message_nor_its_node", func(t *testing.T) {
		seedSessions(t)
		// alice's credential proved HUSH's session (session_owners), so its content rule passes her.
		seed(t, `insert into session_owners (session_id, login, first_at) values ($1, $2, now()) on conflict do nothing`, hush.session, rlsOwner)
		const message = `00000000-0000-4000-8000-00000000d101`
		if _, err := w.deps.Store.Pool.Exec(store.WithViewer(ctx, aliceSees), `insert into messages (id, author, body, target, created_at)
			values ($1, '{"kind":"user","id":"alice"}', 'a direct message', 'session:' || $2, now())`, message, hush.session); err != nil {
			t.Fatalf("alice's insert of a message on no issue whose node is not written yet: %v", err)
		}
		seed(t, `insert into graph_nodes (kind, id, supertag, visibility, state, made_at, made_by, source_event, parties)
			values ('message', $1, 'message', 'project', 'live', now(), '{}', 0, array['person:alice', 'session:' || $2])`, message, hush.session)
		const messages = `select count(*) from messages where id = '00000000-0000-4000-8000-00000000d101'`
		const nodes = `select count(*) from graph_nodes where kind = 'message' and id = '00000000-0000-4000-8000-00000000d101'`
		if hidden, hiddenNode := countAs(t, bobSees, messages), countAs(t, bobSees, nodes); hidden != 0 || hiddenNode != 0 {
			t.Errorf("bob, neither sender nor recipient, reads the message %d times and its node %d, want neither", hidden, hiddenNode)
		}
		if seen, seenNode := countAs(t, aliceSees, messages), countAs(t, aliceSees, nodes); seen != 1 || seenNode != 1 {
			t.Errorf("alice, its sender, reads the message %d times and its node %d, want each once", seen, seenNode)
		}
	})
}
