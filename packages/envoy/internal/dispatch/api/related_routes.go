package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// relatedExcludeLimit is the most issues one no-write related read may leave out. A read that
// leaves out an issue's whole subtree sends one key per issue, and a project's deepest tree is
// far short of this; a longer list is a caller sending the project rather than a neighbourhood.
const relatedExcludeLimit = 50

// relatedRequest is the no-write related route's question, from GET's query or POST's body.
// ExcludeIssue is the single issue released clients send, which is also left out of the word
// statistics; ExcludeIssues is the list, which leaves its issues out of the results alone.
type relatedRequest struct {
	Title         string   `json:"title"`
	Text          string   `json:"text"`
	ExcludeIssue  string   `json:"exclude_issue"`
	ExcludeIssues []string `json:"exclude_issues"`
	KeywordsOnly  bool     `json:"keywords_only"`
}

// checkText is what the check reads: the title and the text, each alone when the other is blank,
// on lines of their own as a new issue's title and spec are.
func (request relatedRequest) checkText() string {
	title, text := strings.TrimSpace(request.Title), strings.TrimSpace(request.Text)
	if title == "" || text == "" {
		return title + text
	}
	return title + "\n" + text
}

// getRelated answers GET /api/v1/projects/{key}/related: the related-item check over the query's
// title and text. They ride in the URL, so they are capped together as `dispatch search` caps q;
// a longer text goes through POST.
func (s *server) getRelated(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	// keywords_only is read as search's own ?keywords_only= is (parseQueryBool), in the reading
	// this route has always given it: the first value as it stands, untrimmed, and no value or a
	// blank one asks for the check both legs make.
	keywordsOnly, present, valid := parseQueryBool(query, "keywords_only", false, true)
	if present && !valid {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "keywords_only must be true or false")
		return
	}
	request := relatedRequest{
		Title:         query.Get("title"),
		Text:          query.Get("text"),
		ExcludeIssue:  query.Get("exclude_issue"),
		ExcludeIssues: query["exclude_issues"],
		KeywordsOnly:  keywordsOnly,
	}
	if length := itemblocks.Units16(request.checkText()); length > contracts.SearchQueryMax {
		tooLong := capExceededError("title and text", length, contracts.SearchQueryMax)
		writeError(w, tooLong.code, tooLong.status, tooLong.message+"; send a longer text with POST")
		return
	}
	// The GET form offers no answered question of another project: its one caller is the
	// new-issue dialog of the web application, which runs the check for each burst of a person's
	// typing and draws the items of the asking project alone.
	s.answerRelated(w, r, request, false)
}

// postRelated answers POST /api/v1/projects/{key}/related: the check over the body's title and
// text. The title is capped as an issue's is, and the text, like a new issue's spec, by the body's
// own bound (decodeJSON's maxJSONRequestBytes).
func (s *server) postRelated(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var request relatedRequest
	if err := decodeJSON(r, &request); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if length := itemblocks.Units16(request.Title); length > contracts.IssueTitleMax {
		capExceeded(w, "title", length, contracts.IssueTitleMax)
		return
	}
	// The POST form is what `dispatch related` calls, which an agent runs on purpose to ask a
	// question, so it offers the answered questions of the other projects too.
	s.answerRelated(w, r, request, true)
}

// answerRelated runs the check both forms share against the path's project, recording nothing:
// it has no source to record, and a reader trying words out must not count as a write's check.
// question says the form asks a person a question, which the two forms do not do alike
// (postRelated and getRelated say why).
func (s *server) answerRelated(w http.ResponseWriter, r *http.Request, request relatedRequest, question bool) {
	project := r.PathValue("key")
	if !projectKeyPattern.MatchString(project) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("key", project))
		return
	}
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	request.ExcludeIssue = strings.TrimSpace(request.ExcludeIssue)
	if request.ExcludeIssue != "" {
		if err := s.requireProjectIssue(r, project, request.ExcludeIssue); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	excluded := make([]string, 0, len(request.ExcludeIssues))
	for _, key := range request.ExcludeIssues {
		if key = strings.TrimSpace(key); key != "" {
			excluded = append(excluded, key)
		}
	}
	if err := s.requireProjectIssues(r, project, excluded); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The answered asks this check offers carry the display tree of their answer for a caller
	// that asked for the trees, as the same answer does in a search hit: the dashboard's related
	// panel reads that tree rather than parsing the markdown itself. The reader is resolved
	// before the response is written, which is where every tree field is filled.
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	suggestions := s.relatedCheck(r.Context(), relatedInput{
		project:       project,
		text:          request.checkText(),
		title:         request.Title,
		excludeIssue:  request.ExcludeIssue,
		excludeIssues: excluded,
		keywordsOnly:  request.KeywordsOnly,
		question:      question,
		trees:         reader,
	})
	reader.resolve()
	WriteJSON(w, http.StatusOK, suggestions)
}

// requireProjectIssue refuses an exclude_issue that is not an issue of project, as a create
// refuses a parent it cannot find or that sits in another project.
func (s *server) requireProjectIssue(r *http.Request, project, key string) error {
	if !issueKeyPattern.MatchString(key) {
		return errorf(http.StatusBadRequest, "EXCLUDE_ISSUE_INPUT", "exclude_issue %q is not an issue key", key)
	}
	var issueProject string
	err := s.deps.Store.Pool.QueryRow(r.Context(), `select project_key from issues where key = $1`, key).Scan(&issueProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return errorf(http.StatusBadRequest, "EXCLUDE_ISSUE_INPUT", "exclude_issue issue %s not found", key)
	}
	if err != nil {
		return err
	}
	if issueProject != project {
		return errorf(http.StatusBadRequest, "EXCLUDE_ISSUE_INPUT", "exclude_issue %s is not an issue of project %s", key, project)
	}
	return nil
}

// requireProjectIssues refuses an exclusion list longer than relatedExcludeLimit, and one
// holding anything that is not an issue of project. Every key is read in one statement, so a
// list of fifty costs one round trip, and the refusal names the first key the project does not
// hold rather than the whole list, as the single exclude_issue's does.
func (s *server) requireProjectIssues(r *http.Request, project string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if len(keys) > relatedExcludeLimit {
		return errorf(http.StatusBadRequest, "EXCLUDE_ISSUE_INPUT",
			"exclude_issues takes at most %d issues; %d were sent", relatedExcludeLimit, len(keys))
	}
	for _, key := range keys {
		if !issueKeyPattern.MatchString(key) {
			return errorf(http.StatusBadRequest, "EXCLUDE_ISSUE_INPUT", "exclude_issues %q is not an issue key", key)
		}
	}
	var missing []string
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		select coalesce(array_agg(k.key order by k.ordinality), '{}')
		  from unnest($1::text[]) with ordinality k(key, ordinality)
		 where not exists (select 1 from issues i where i.key = k.key and i.project_key = $2)
	`, keys, project).Scan(&missing); err != nil {
		return err
	}
	if len(missing) > 0 {
		return errorf(http.StatusBadRequest, "EXCLUDE_ISSUE_INPUT",
			"exclude_issues %s is not an issue of project %s", missing[0], project)
	}
	return nil
}

// relatedChecksReportQuery counts the related_checks rows the filters select ($1 project, $2
// from, $3 to, $4 source kind; each null for no filter). The percentiles are percentile_disc,
// which ignores nulls like every aggregate here, so each is a time some check took rather than
// one interpolated between two checks.
const relatedChecksReportQuery = `
with checks as (
  select source_kind, outcome, search, reason, keyword_ms, total_ms
    from related_checks
   where ($1::text is null or project_key = $1)
     and ($2::timestamptz is null or created_at >= $2)
     and ($3::timestamptz is null or created_at < $3)
     and ($4::text is null or source_kind = $4)
)
select
  (select count(*) from checks),
  (select coalesce(jsonb_object_agg(source_kind, n), '{}') from (select source_kind, count(*) as n from checks group by 1) c),
  (select coalesce(jsonb_object_agg(outcome, n), '{}') from (select outcome, count(*) as n from checks group by 1) c),
  (select coalesce(jsonb_object_agg(mode, n), '{}') from (select coalesce(search, 'not_made') as mode, count(*) as n from checks group by 1) c),
  (select coalesce(jsonb_object_agg(reason, n), '{}') from (select reason, count(*) as n from checks where reason is not null group by 1) c),
  (select count(*) from checks where outcome in ('found', 'none')),
  (select count(*) from checks where reason = 'timeout'),
  count(keyword_ms), percentile_disc(0.5) within group (order by keyword_ms),
  percentile_disc(0.95) within group (order by keyword_ms), max(keyword_ms), count(*) filter (where keyword_ms > 300),
  count(total_ms), percentile_disc(0.5) within group (order by total_ms),
  percentile_disc(0.95) within group (order by total_ms), max(total_ms), count(*) filter (where total_ms > 300)
from checks`

// relatedChecksOffersQuery counts the writes whose check offered an answered question of another
// project, under the filters relatedChecksReportQuery applies to related_checks ($1 project, $2
// from, $3 to, $4 source kind; each null for no filter).
//
// It counts the writes, not the items: a check that offered two questions wrote two rows of the
// role and is one write, so the count is over the distinct ask each row names. Every row of the
// role holds that ask, because only an ask create records one (persistSuggestions), and
// count(distinct) passes over a null, so a source kind no row of the role carries counts zero.
//
// A row names its source by the issue or the project document it sits on, never by a project, so
// the project filter reads the project of that owner: issues.project_key for a row with a
// source_issue_key, artifacts.project_key for one with a source_artifact_id. The row's own
// created_at is when it was recorded, which is the window the report's other counts read too.
const relatedChecksOffersQuery = `
select count(distinct ws.source_ask_id)
  from write_suggestions ws
  left join issues i on i.key = ws.source_issue_key
  left join artifacts a on a.id = ws.source_artifact_id
 where ws.role = 'other_project'
   and ($1::text is null or coalesce(i.project_key, a.project_key) = $1)
   and ($2::timestamptz is null or ws.created_at >= $2)
   and ($3::timestamptz is null or ws.created_at < $3)
   and ($4::text is null or ws.source_kind = $4)`

// relatedSourceKinds are related_checks.source_kind's values, the records route's source_kind
// filter.
var relatedSourceKinds = map[string]bool{"issue": true, "ask": true, "decision_block": true}

// getRelatedChecks answers GET /api/v1/related-checks: model.RelatedChecksReport over the checks
// recorded within [from, to), of one project and one source kind when given. The counts of
// related_checks come in one statement and OtherProjectOffers, which write_suggestions holds, in
// a second one over the same window and filters (relatedChecksOffersQuery).
func (s *server) getRelatedChecks(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	var report model.RelatedChecksReport
	if project := strings.TrimSpace(query.Get("project")); project != "" {
		if !projectKeyPattern.MatchString(project) {
			writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("project", project))
			return
		}
		report.Project = &project
	}
	if kind := strings.TrimSpace(query.Get("source_kind")); kind != "" {
		if !relatedSourceKinds[kind] {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, fmt.Sprintf("source_kind %q is not issue, ask or decision_block", kind))
			return
		}
		report.SourceKind = &kind
	}
	for _, bound := range []struct {
		name   string
		target **time.Time
	}{{"from", &report.From}, {"to", &report.To}} {
		if strings.TrimSpace(query.Get(bound.name)) == "" {
			continue
		}
		parsed, err := parseOptionalRFC3339(query, bound.name, time.Time{})
		if err != nil {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
			return
		}
		*bound.target = &parsed
	}
	if report.From != nil && report.To != nil && !report.From.Before(*report.To) {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "from must be before to")
		return
	}

	keyword, total := &report.KeywordMS, &report.TotalMS
	if err := s.deps.Store.Pool.QueryRow(r.Context(), relatedChecksReportQuery,
		report.Project, report.From, report.To, report.SourceKind,
	).Scan(
		&report.Total, &report.BySourceKind, &report.ByOutcome, &report.BySearch, &report.ByReason,
		&report.Completed, &report.SkippedForTime,
		&keyword.Count, &keyword.P50, &keyword.P95, &keyword.Max, &keyword.Over300MS,
		&total.Count, &total.P50, &total.P95, &total.Max, &total.Over300MS,
	); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.deps.Store.Pool.QueryRow(r.Context(), relatedChecksOffersQuery,
		report.Project, report.From, report.To, report.SourceKind,
	).Scan(&report.OtherProjectOffers); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, report)
}
