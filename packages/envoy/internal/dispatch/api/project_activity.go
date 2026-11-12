package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The weeks a project-activity read spans: projectActivityWeeksDefault when the caller names
// none, and at most projectActivityWeeksMax, a year of ISO weeks.
const (
	projectActivityWeeksDefault = 8
	projectActivityWeeksMax     = 52
)

// projectActivity is GET /api/v1/projects/{key}/activity: the events of the project's issues
// counted per ISO week, in total and by kind, over the last weeks, oldest first and this week
// last. Every week of the span is listed, a week with no event as zero, so a series drawn from it
// has a point for each week.
type projectActivity struct {
	Project string                `json:"project"`
	Weeks   []projectActivityWeek `json:"weeks"`
}

// projectActivityWeek is one ISO week: its name (2026-W45), the moment it starts (Monday 00:00
// UTC), how many events the project's issues recorded in it, and those events counted by kind,
// each kind an event type such as issue.created or comment.created.
type projectActivityWeek struct {
	Week   string         `json:"week"`
	Start  time.Time      `json:"start"`
	Events int            `json:"events"`
	Kinds  map[string]int `json:"kinds"`
}

// projectActivityStatement reads the span's ISO weeks - this week and the weeks before it, Monday
// 00:00 UTC each, by the database's clock - and every event a project issue recorded in each, $1
// the project and $2 the number of weeks. A week with no event answers one row with a null kind,
// which the read counts as none.
const projectActivityStatement = `
	with span as (
		select week from generate_series(
			date_trunc('week', now() at time zone 'UTC') - make_interval(weeks => $2 - 1),
			date_trunc('week', now() at time zone 'UTC'),
			interval '1 week'
		) as week
	)
	select to_char(span.week, 'IYYY-"W"IW'), span.week at time zone 'UTC', e.type, count(e.id)::int
	from span
	left join (
		select e.id, e.type, e.created_at
		from events e
		join issues i on i.key = e.issue_key
		where i.project_key = $1
			and e.created_at >= (date_trunc('week', now() at time zone 'UTC') - make_interval(weeks => $2 - 1)) at time zone 'UTC'
	) e on e.created_at >= span.week at time zone 'UTC'
		and e.created_at < (span.week + interval '1 week') at time zone 'UTC'
	group by span.week, e.type
	order by span.week, e.type`

// getProjectActivity answers GET /api/v1/projects/{key}/activity?weeks=N: the events of the
// project's issues counted by kind per ISO week over the last N weeks (default 8, 1 to 52). A key
// that is no project key is 400 INVALID_PROJECT, a project Dispatch does not hold 404
// PROJECT_NOT_FOUND, and a weeks that is not an integer from 1 to 52 400 INVALID_QUERY.
func (s *server) getProjectActivity(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	project := r.PathValue("key")
	if !projectKeyPattern.MatchString(project) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("key", project))
		return
	}
	weeks := projectActivityWeeksDefault
	if r.URL.Query().Has("weeks") {
		parsed, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("weeks")))
		if err != nil || parsed < 1 || parsed > projectActivityWeeksMax {
			writeError(w, "INVALID_QUERY", http.StatusBadRequest, "weeks must be an integer from 1 to 52")
			return
		}
		weeks = parsed
	}
	activity, err := s.projectActivity(r.Context(), project, weeks)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, activity)
}

// projectActivity is the activity of project over the last weeks ISO weeks, and a 404
// PROJECT_NOT_FOUND apiError for a project Dispatch does not hold. The route and the going run's
// activity read (going_reads.go) answer through it alike.
func (s *server) projectActivity(ctx context.Context, project string, weeks int) (projectActivity, error) {
	pool := s.deps.Store.Pool
	if _, err := s.loadProject(ctx, pool, project); err != nil {
		return projectActivity{}, err
	}
	rows, err := pool.Query(ctx, projectActivityStatement, project, weeks)
	if err != nil {
		return projectActivity{}, err
	}
	defer rows.Close()
	activity := projectActivity{Project: project, Weeks: make([]projectActivityWeek, 0, weeks)}
	for rows.Next() {
		var week string
		var start time.Time
		var kind *string
		var count int
		if err := rows.Scan(&week, &start, &kind, &count); err != nil {
			return projectActivity{}, err
		}
		if len(activity.Weeks) == 0 || activity.Weeks[len(activity.Weeks)-1].Week != week {
			activity.Weeks = append(activity.Weeks, projectActivityWeek{Week: week, Start: start.UTC(), Kinds: map[string]int{}})
		}
		if kind == nil {
			continue
		}
		current := &activity.Weeks[len(activity.Weeks)-1]
		current.Kinds[*kind] = count
		current.Events += count
	}
	return activity, rows.Err()
}
