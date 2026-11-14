package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// offerRow is one write_suggestions row as the table holds it, every column a case reads.
type offerRow struct {
	ID                string
	CreatedAt         time.Time
	SourceKind        string
	SourceIssueKey    *string
	SourceArtifactID  *string
	SourceAskID       *string
	ActorKind         string
	ActorID           string
	Role              string
	Rank              int
	SuggestedKind     string
	SuggestedID       string
	SuggestedIssueKey *string
	Outcome           string
	OutcomeDetail     *string
	Swept             bool
}

// String prints the row as the table holds it, so what a case logs is the record itself.
func (row offerRow) String() string {
	return fmt.Sprintf("write_suggestions{id=%s created_at=%s source_kind=%s source_issue_key=%s "+
		"source_artifact_id=%s source_ask_id=%s actor=%s/%s role=%s rank=%d suggested_kind=%s "+
		"suggested_id=%s suggested_issue_key=%s outcome=%s outcome_detail=%s swept=%t}",
		row.ID, row.CreatedAt.UTC().Format(time.RFC3339Nano), row.SourceKind, offerText(row.SourceIssueKey),
		offerText(row.SourceArtifactID), offerText(row.SourceAskID), row.ActorKind, row.ActorID, row.Role,
		row.Rank, row.SuggestedKind, row.SuggestedID, offerText(row.SuggestedIssueKey), row.Outcome,
		offerText(row.OutcomeDetail), row.Swept)
}

// offerText prints a nullable column as the table holds it.
func offerText(value *string) string {
	if value == nil {
		return "null"
	}
	return *value
}

// readOfferRows reads the write_suggestions rows where selects, oldest role and rank first.
func readOfferRows(t *testing.T, database *store.Store, where string, args ...any) []offerRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select id::text, created_at, source_kind, source_issue_key, source_artifact_id::text, source_ask_id::text,
		       actor_kind, actor_id, role, rank, suggested_kind, suggested_id, suggested_issue_key,
		       outcome, outcome_detail, swept
		  from write_suggestions
		 where `+where+`
		 order by role, rank`, args...)
	if err != nil {
		t.Fatalf("read write_suggestions where %s: %v", where, err)
	}
	defer rows.Close()
	var read []offerRow
	for rows.Next() {
		var row offerRow
		if err := rows.Scan(&row.ID, &row.CreatedAt, &row.SourceKind, &row.SourceIssueKey, &row.SourceArtifactID,
			&row.SourceAskID, &row.ActorKind, &row.ActorID, &row.Role, &row.Rank, &row.SuggestedKind,
			&row.SuggestedID, &row.SuggestedIssueKey, &row.Outcome, &row.OutcomeDetail, &row.Swept); err != nil {
			t.Fatalf("scan a write_suggestions row: %v", err)
		}
		read = append(read, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read write_suggestions where %s: %v", where, err)
	}
	return read
}

// offerRanks is the rank of each row, in the order the rows stand.
func offerRanks(rows []offerRow) []int {
	ranks := make([]int, len(rows))
	for index, row := range rows {
		ranks[index] = row.Rank
	}
	return ranks
}

// offerSuggestedIDs is the item each row names, in the order the rows stand.
func offerSuggestedIDs(rows []offerRow) []string {
	ids := make([]string, len(rows))
	for index, row := range rows {
		ids[index] = row.SuggestedID
	}
	return ids
}

// offerRowsByRole counts the write_suggestions rows the database holds under each role.
func offerRowsByRole(t *testing.T, database *store.Store) map[string]int {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select role, count(*) from write_suggestions group by role`)
	if err != nil {
		t.Fatalf("count write_suggestions by role: %v", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var role string
		var count int
		if err := rows.Scan(&role, &count); err != nil {
			t.Fatalf("scan a role count: %v", err)
		}
		counts[role] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count write_suggestions by role: %v", err)
	}
	return counts
}

// offerFixture is what the recording cases check with: the asking project, which crossProjectAsker
// seeds, and one other project holding the answered questions the check offers.
type offerFixture struct {
	handler      http.Handler
	database     *store.Store
	deps         Deps
	host         string   // the issue of the asking project the question is asked on
	hostArtifact string   // that issue's primary document, where a decision block is added
	owner        string   // the issue of the other project holding the answered questions
	offered      []string // those questions, the nearest the check's text first
}

// seedCrossProjectOffers seeds the two projects and count answered questions of the other one,
// the first at cosine 0.90 and the second at 0.80, so a check of the asking project offers them
// in that order (suggestionOtherProjectCount is 2).
func seedCrossProjectOffers(t *testing.T, asking, other string, count int) offerFixture {
	t.Helper()
	embedder := newCrossProjectEmbedder()
	for _, text := range crossProjectQueryTexts() {
		embedder.vectors[text] = queryVector()
	}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	_, _, host, hostArtifact := crossProjectAsker(t, handler, database, embedder, asking)
	createSuggestionProject(t, handler, other)
	owner := createSearchIssue(t, handler, other, "Backlog runbook", "What to do when the backlog grows.")
	seeds := []struct {
		question, answer string
		similarity       float64
		axis             int
	}{
		{"When the backlog grows, which store do we empty first?",
			"Empty the page store first; the index store costs more to rebuild.", 0.9, 2},
		{"Which buffer do we empty when the backlog grows?",
			"Empty the write buffer; the read buffer refills itself.", 0.8, 3},
	}
	offered := make([]string, 0, count)
	for _, seed := range seeds[:count] {
		offered = append(offered, seedAnsweredQuestion(t, handler, database, embedder, owner,
			seed.question, seed.answer, seed.similarity, seed.axis))
	}
	refreshTermStats(t, database, asking)
	processAllPending(t, database, embedder)
	return offerFixture{
		handler: handler, database: database, deps: deps,
		host: host, hostArtifact: hostArtifact, owner: owner, offered: offered,
	}
}

// askTheQuestion opens the ask every recording case asks, on the issue or the project document
// target names, and answers what the write offered and the ask's own id.
func askTheQuestion(t *testing.T, fixture offerFixture, path string) (*model.Suggestions, string) {
	t.Helper()
	response := dispatchRequest(t, fixture.handler, http.MethodPost, path,
		map[string]any{"question": crossProjectQuestion}, "alice")
	return crossAdvice(t, "the ask create", response), decodeBody[model.Ask](t, response).ID
}

// An ask create on an issue records every answered question of another project it offered: one
// row of the role other_project per item, ranked in the order the items stand, each naming the
// ask it offers and the issue that holds it, beside the related rows of the same check.
func TestSuggestionOffersOfAnAskCreateAreRankedRowsOfTheirOwnRole(t *testing.T) {
	fixture := seedCrossProjectOffers(t, "XREC", "XRECO", 2)

	offered, askID := askTheQuestion(t, fixture, "/api/v1/issues/"+fixture.host+"/asks")
	if ids := crossProjectItemIDs(offered.OtherProjects); !slices.Equal(ids, fixture.offered) {
		t.Fatalf("the ask create offered %v, want the two answered questions of XRECO, %v", ids, fixture.offered)
	}

	rows := readOfferRows(t, fixture.database, "source_ask_id = $1 and role = 'other_project'", askID)
	if !slices.Equal(offerSuggestedIDs(rows), fixture.offered) || !slices.Equal(offerRanks(rows), []int{0, 1}) {
		t.Fatalf("recorded %v at ranks %v, want %v at ranks [0 1]", offerSuggestedIDs(rows), offerRanks(rows), fixture.offered)
	}
	for _, row := range rows {
		if row.SourceKind != "ask" || offerText(row.SourceIssueKey) != fixture.host || row.SourceArtifactID != nil ||
			offerText(row.SourceAskID) != askID {
			t.Fatalf("row %s, want it sourced on the ask %s of the issue %s with no document", row, askID, fixture.host)
		}
		if row.SuggestedKind != "ask" || offerText(row.SuggestedIssueKey) != fixture.owner {
			t.Fatalf("row %s, want an ask of the issue %s", row, fixture.owner)
		}
		if row.ActorKind != "user" || row.ActorID != "alice" || row.Outcome != "ignored" || row.OutcomeDetail != nil || row.Swept {
			t.Fatalf("row %s, want alice's row pending, unswept and with no detail", row)
		}
	}
	related := readOfferRows(t, fixture.database, "source_ask_id = $1 and role = 'related'", askID)
	if len(related) != len(offered.Related) || len(related) == 0 {
		t.Fatalf("the same check recorded %d related rows for %d related items, want one each",
			len(related), len(offered.Related))
	}
	t.Logf("measured: the ask create %s offered %v and recorded them as\n\t%s\n\t%s\nbeside %d row(s) of the role related",
		askID, fixture.offered, rows[0], rows[1], len(related))
}

// An ask create on a project document records its offers too: the row carries the document in
// place of an issue, which is the owner such an ask has.
func TestSuggestionOffersOfAnAskOnAProjectDocumentAreRecorded(t *testing.T) {
	fixture := seedCrossProjectOffers(t, "XDOC", "XDOCO", 1)
	document := uploadProjectDocument(t, fixture.handler, "XDOC", "backlog-notes.md",
		"# Backlog notes\n\nWhat the weekly planning meeting decided.\n")

	offered, askID := askTheQuestion(t, fixture, "/api/v1/artifacts/"+document+"/asks")
	if ids := crossProjectItemIDs(offered.OtherProjects); !slices.Equal(ids, fixture.offered) {
		t.Fatalf("the document ask offered %v, want %v", ids, fixture.offered)
	}

	rows := readOfferRows(t, fixture.database, "source_ask_id = $1 and role = 'other_project'", askID)
	if len(rows) != 1 || rows[0].Rank != 0 || rows[0].SuggestedID != fixture.offered[0] {
		t.Fatalf("recorded %v, want the one offered question %s at rank 0", rows, fixture.offered[0])
	}
	if rows[0].SourceKind != "ask" || rows[0].SourceIssueKey != nil || offerText(rows[0].SourceArtifactID) != document {
		t.Fatalf("row %s, want it sourced on the project document %s and on no issue", rows[0], document)
	}
	t.Logf("measured: the ask %s on the project document %s of XDOC offered %s and recorded\n\t%s",
		askID, document, fixture.offered[0], rows[0])
}

// A new decision block offers the answered question of the other project and records no row of
// it: the table keys a row of the role by the ask it was offered on, and such a check holds the
// document block's own id and no ask id (persistSuggestions).
func TestSuggestionOffersOfANewDecisionBlockReachNoRow(t *testing.T) {
	fixture := seedCrossProjectOffers(t, "XBLOCK", "XBLOCKO", 1)
	before := readOfferRows(t, fixture.database, "role = 'other_project'")

	checks, present := decisionBlockChecksOf(t, editDocument(t, fixture.handler, fixture.hostArtifact,
		insertAtEnd(":::ask{#evict}\n"+crossProjectQuestion+"\n:::")))
	if !present || len(checks) != 1 || checks[0].Suggestions == nil {
		t.Fatalf("decision_block_checks = %+v (present %t), want one with its suggestions", checks, present)
	}
	if ids := crossProjectItemIDs(checks[0].Suggestions.OtherProjects); !slices.Equal(ids, fixture.offered) {
		t.Fatalf("the decision block offered %v, want %v: the case measures a check that had an offer to record",
			ids, fixture.offered)
	}

	after := readOfferRows(t, fixture.database, "role = 'other_project'")
	if len(after) != len(before) {
		t.Fatalf("the rows of the role went from %d to %d, want the block's offer recorded nowhere: %v", len(before), len(after), after)
	}
	blocks := readOfferRows(t, fixture.database, "role = 'other_project' and source_ask_id is null")
	if len(blocks) != 0 {
		t.Fatalf("write_suggestions holds %v of the role with no ask, want the block's offer recorded nowhere", blocks)
	}
	t.Logf("measured: the decision block offered %s and left write_suggestions at %d row(s) of the role other_project, "+
		"each of them an ask create's", fixture.offered[0], len(after))
}

// The check writes no row of the role decision any more: the roles an ask create records are
// related and other_project alone, though the table still admits the rows earlier checks wrote.
func TestSuggestionRolesOfAnAskCreateLeaveNoDecisionRow(t *testing.T) {
	fixture := seedCrossProjectOffers(t, "XROLE", "XROLEO", 1)

	askTheQuestion(t, fixture, "/api/v1/issues/"+fixture.host+"/asks")
	createSearchIssue(t, fixture.handler, "XROLE", "Another weekly rota question", "The rota slips again.")

	counts := offerRowsByRole(t, fixture.database)
	if counts["decision"] != 0 {
		t.Fatalf("write_suggestions holds %d rows of the role decision, want none", counts["decision"])
	}
	if counts["related"] == 0 || counts["other_project"] == 0 {
		t.Fatalf("roles recorded = %v, want rows under related and other_project: a database with none would "+
			"say nothing about the role decision", counts)
	}
	t.Logf("measured: after an ask create and an issue create the roles recorded are %v", counts)
}

// The outcome sweep advances a row of the new role as it advances every other: it reads no role
// (RunSuggestionOutcomeSweep). With no later activity the row is marked swept and reads ignored,
// which is also the column's default, so the swept mark is what says the sweep reached it. The
// records route then counts the write that offered it.
func TestSuggestionOfferIsSweptAndStaysIgnoredWithoutLaterActivity(t *testing.T) {
	fixture := seedCrossProjectOffers(t, "XSWEEP", "XSWEEPO", 1)
	srv := directServer(fixture.deps)
	from := time.Now().Add(-time.Minute)

	_, askID := askTheQuestion(t, fixture, "/api/v1/issues/"+fixture.host+"/asks")
	recorded := readOfferRows(t, fixture.database, "source_ask_id = $1 and role = 'other_project'", askID)
	if len(recorded) != 1 || recorded[0].Swept || recorded[0].Outcome != "ignored" {
		t.Fatalf("recorded %v, want one row pending and unswept", recorded)
	}

	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	swept := readOfferRows(t, fixture.database, "id = $1", recorded[0].ID)
	if len(swept) != 1 || !swept[0].Swept || swept[0].Outcome != "ignored" || swept[0].OutcomeDetail != nil {
		t.Fatalf("after the sweep the row is %v, want it swept, still ignored and with no detail", swept)
	}

	to := time.Now().Add(time.Minute)
	stamp := func(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }
	report, body := getRelatedChecks(t, fixture.handler, url.Values{
		"project": {"XSWEEP"}, "from": {stamp(from)}, "to": {stamp(to)},
	})
	if report.OtherProjectOffers != 1 {
		t.Fatalf("other_project_offers = %d, want 1 for the window holding the row: %s", report.OtherProjectOffers, body)
	}
	t.Logf("measured: the recorded row is\n\t%s\nand after the sweep\n\t%s\nthe report of XSWEEP over [%s, %s) reads %s",
		recorded[0], swept[0], stamp(from), stamp(to), body)
}

// A later write of the same actor that cites the offered ask from the source issue marks the row
// acted_on, the rule every other role is judged by (suggestionActedOnByCitation).
func TestSuggestionOfferBecomesActedOnWhenTheSourceCitesTheOfferedAsk(t *testing.T) {
	fixture := seedCrossProjectOffers(t, "XCITE", "XCITEO", 1)
	srv := directServer(fixture.deps)

	_, askID := askTheQuestion(t, fixture, "/api/v1/issues/"+fixture.host+"/asks")
	recorded := readOfferRows(t, fixture.database, "source_ask_id = $1 and role = 'other_project'", askID)
	if len(recorded) != 1 {
		t.Fatalf("recorded %v, want the one offered question", recorded)
	}
	time.Sleep(10 * time.Millisecond) // the citation must postdate the row

	citation := "dispatch://" + fixture.owner + "/ask/" + fixture.offered[0]
	postOutcomeMessage(t, fixture.handler, fixture.host, "alice", "XCITEO settled this already: "+citation)

	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	judged := readOfferRows(t, fixture.database, "id = $1", recorded[0].ID)
	if len(judged) != 1 || judged[0].Outcome != "acted_on" || !judged[0].Swept {
		t.Fatalf("after the sweep the row is %v, want it acted_on and swept", judged)
	}
	t.Logf("measured: the message citing %s on %s made the row acted_on, %q:\n\t%s",
		citation, fixture.host, offerText(judged[0].OutcomeDetail), judged[0])
}

// seededOffer is one write_suggestions row a report case writes directly, so the window, the
// source and the role it counts are exactly what it seeded.
type seededOffer struct {
	role        string
	sourceKind  string
	issueKey    string // the source issue, empty for a row sourced on a project document
	artifactID  string // the source project document, empty for a row sourced on an issue
	askID       string // the ask the check ran for, empty for an issue source
	rank        int
	suggestedID string
	at          time.Time
}

// seedWriteSuggestionOffers replaces every write_suggestions row (the fixture's own writes
// recorded some) with offers.
func seedWriteSuggestionOffers(t *testing.T, database *store.Store, offers []seededOffer) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), "delete from write_suggestions"); err != nil {
		t.Fatalf("clear write_suggestions: %v", err)
	}
	for _, offer := range offers {
		if _, err := database.Pool.Exec(context.Background(), `
			insert into write_suggestions
				(created_at, source_kind, source_issue_key, source_artifact_id, source_ask_id, actor_kind, actor_id,
				 role, rank, suggested_kind, suggested_id)
			values ($1, $2, $3, $4, $5, 'user', 'alice', $6, $7, 'ask', $8)
		`, offer.at, offer.sourceKind, nullableText(offer.issueKey), nullableText(offer.artifactID),
			nullableText(offer.askID), offer.role, offer.rank, offer.suggestedID); err != nil {
			t.Fatalf("seed write_suggestions %+v: %v", offer, err)
		}
	}
}

// offerReportFixture is the sources the report cases seed rows on: an issue and a project
// document of one project, an issue of another, and one ask on each of the three.
type offerReportFixture struct {
	handler                         http.Handler
	database                        *store.Store
	issue, document, otherIssue     string
	issueAsk, documentAsk, otherAsk string
}

func seedOfferReportSources(t *testing.T, project, other string) offerReportFixture {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, project)
	createSuggestionProject(t, handler, other)
	issue := createSearchIssue(t, handler, project, "Offer report fixture", "")
	otherIssue := createSearchIssue(t, handler, other, "Other offer report fixture", "")
	document := uploadProjectDocument(t, handler, project, "offers.md", "# Offers\n\nPlanning notes.\n")
	documentAsk := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document+"/asks",
		map[string]any{"question": "Which store do we empty first?"}, "alice")
	if documentAsk.Code != http.StatusCreated {
		t.Fatalf("open the document ask: status=%d body=%s", documentAsk.Code, documentAsk.Body.String())
	}
	return offerReportFixture{
		handler: handler, database: database,
		issue: issue, document: document, otherIssue: otherIssue,
		issueAsk:    createSearchAsk(t, handler, issue, "Which cache do we drop first?", "Yes", "No"),
		documentAsk: decodeBody[model.Ask](t, documentAsk).ID,
		otherAsk:    createSearchAsk(t, handler, otherIssue, "Which queue do we drain first?", "Yes", "No"),
	}
}

// reportOffers is the rows the three report cases share: one ask create of the issue that
// offered two questions (two rows, one write), one of the project document that offered one, one
// of the other project outside the window, and a related row of the same issue ask, which no
// count of the role may read.
func reportOffers(fixture offerReportFixture, hour func(int) time.Time) []seededOffer {
	return []seededOffer{
		{"other_project", "ask", fixture.issue, "", fixture.issueAsk, 0, "offer-a", hour(1)},
		{"other_project", "ask", fixture.issue, "", fixture.issueAsk, 1, "offer-b", hour(1)},
		{"related", "ask", fixture.issue, "", fixture.issueAsk, 0, "related-a", hour(1)},
		{"other_project", "ask", "", fixture.document, fixture.documentAsk, 0, "offer-c", hour(2)},
		{"other_project", "ask", fixture.otherIssue, "", fixture.otherAsk, 0, "offer-d", hour(30)},
	}
}

// reportHours is the window the report cases count over: an hour count from one fixed start.
func reportHours() (start time.Time, hour func(int) time.Time) {
	start = time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	return start, func(n int) time.Time { return start.Add(time.Duration(n) * time.Hour) }
}

func reportStamp(at time.Time) string { return at.Format(time.RFC3339) }

// The records route counts the writes that offered an answered question of another project, not
// the questions they offered, over the window its other counts read: from inclusive, to
// exclusive.
func TestRelatedChecksReportCountsTheOffersInsideItsWindow(t *testing.T) {
	fixture := seedOfferReportSources(t, "ROFF", "ROFFB")
	start, hour := reportHours()
	seedWriteSuggestionOffers(t, fixture.database, reportOffers(fixture, hour))

	for _, window := range []struct {
		name   string
		params url.Values
		offers int
	}{
		{"every project, every time: three writes offered one", url.Values{}, 3},
		{"the first day: the issue ask's two rows are one write, the document ask's is another",
			url.Values{"from": {reportStamp(start)}, "to": {reportStamp(hour(24))}}, 2},
		{"to is exclusive", url.Values{"from": {reportStamp(start)}, "to": {reportStamp(hour(2))}}, 1},
		{"from is inclusive", url.Values{"from": {reportStamp(hour(2))}, "to": {reportStamp(hour(3))}}, 1},
		{"from alone, past the first day", url.Values{"from": {reportStamp(hour(24))}}, 1},
		{"a window holding nothing", url.Values{"from": {reportStamp(hour(3))}, "to": {reportStamp(hour(24))}}, 0},
	} {
		report, body := getRelatedChecks(t, fixture.handler, window.params)
		if report.OtherProjectOffers != window.offers {
			t.Errorf("%s (%v): other_project_offers = %d, want %d: %s",
				window.name, window.params, report.OtherProjectOffers, window.offers, body)
			continue
		}
		t.Logf("measured: %s (%v) counted %d write(s)", window.name, window.params, report.OtherProjectOffers)
	}
}

// The project filter reads the project of the row's own source: the issue for a row that names
// one, the project document for a row that names one instead.
func TestRelatedChecksReportCountsTheOffersOfOneProject(t *testing.T) {
	fixture := seedOfferReportSources(t, "RPROJ", "RPROJB")
	_, hour := reportHours()
	seedWriteSuggestionOffers(t, fixture.database, reportOffers(fixture, hour))

	for _, project := range []struct {
		key    string
		offers int
	}{{"RPROJ", 2}, {"RPROJB", 1}, {"NOPE", 0}} {
		report, body := getRelatedChecks(t, fixture.handler, url.Values{"project": {project.key}})
		if report.OtherProjectOffers != project.offers {
			t.Errorf("project %s: other_project_offers = %d, want %d: %s", project.key, report.OtherProjectOffers, project.offers, body)
			continue
		}
		t.Logf("measured: project %s counted %d write(s)", project.key, report.OtherProjectOffers)
	}
	t.Logf("measured: of RPROJ's two writes one is sourced on the issue %s and one on the project document %s, "+
		"so the filter read the project through each owner in turn", fixture.issue, fixture.document)
}

// Only an ask create records a row of the role, so the count is zero under every other source
// kind, though the same window holds three such writes under the kind ask.
func TestRelatedChecksReportCountsNoOfferUnderASourceKindThatHoldsNone(t *testing.T) {
	fixture := seedOfferReportSources(t, "RKIND", "RKINDB")
	_, hour := reportHours()
	seedWriteSuggestionOffers(t, fixture.database, reportOffers(fixture, hour))

	for _, kind := range []struct {
		name   string
		offers int
	}{{"ask", 3}, {"issue", 0}, {"decision_block", 0}} {
		report, body := getRelatedChecks(t, fixture.handler, url.Values{"source_kind": {kind.name}})
		if report.OtherProjectOffers != kind.offers {
			t.Errorf("source_kind %s: other_project_offers = %d, want %d: %s", kind.name, report.OtherProjectOffers, kind.offers, body)
			continue
		}
		t.Logf("measured: source_kind %s counted %d write(s)", kind.name, report.OtherProjectOffers)
	}
}
