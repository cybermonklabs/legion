package legiontest

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
)

// TestTheKeywordFillReadsEveryEventOfTheSeededHistory: the keyword fill (textversions) reads, of
// each issue, comment and ask event, the fields Dispatch's own event carries. It stops at the
// first event that lacks one it needs, an issue event's title, a comment's id or body, or an ask's
// id or question (a key or issue_key the payload lacks it reads from events.issue_key), and a
// serving process tries that event again, at most a minute apart, so the keyword table never
// fills past it. A history with every shape the rig writes (trees the sampler saw and trees it
// never saw, parked children, merged pull requests and filings of every class), filled once to
// the horizon, has every event decided with no refusal. The fill returns no error, its cursor
// stands at the newest event, each issue's latest keyword version holds the title the rig wrote
// the issue with, and each comment and each ask has its version.
func TestTheKeywordFillReadsEveryEventOfTheSeededHistory(t *testing.T) {
	database := storetest.Open(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	fixture := Seed(t, database.Pool, Spec{Project: "LEGION", Trees: 4, Now: now, Unobserved: 2, Queued: true,
		Parked: true, Merged: true, Filings: true})

	if err := textversions.FillToHorizon(t.Context(), database, events.NewHorizon(database.Pool)); err != nil {
		t.Fatalf("the keyword fill refused the seeded history: %v", err)
	}
	var newest, comments, asks int64
	if err := database.Pool.QueryRow(t.Context(), `
		select coalesce(max(id), 0), count(*) filter (where type = 'comment.created'),
		       count(distinct payload->>'id') filter (where type like 'ask.%')
		  from events`).Scan(&newest, &comments, &asks); err != nil {
		t.Fatalf("read the seeded events: %v", err)
	}
	if newest == 0 || comments == 0 || asks == 0 {
		t.Fatalf("the history holds %d events, %d comments and %d asks, so this test examined too little", newest, comments, asks)
	}
	if cursor := textversionstest.Cursor(t, database); cursor != newest {
		t.Fatalf("the keyword fill's cursor stands at event %d, want the newest event %d", cursor, newest)
	}

	rows, err := database.Pool.Query(t.Context(), `
		select i.key, i.title, v.body
		  from issues i
		  left join lateral (select body from search_text_versions
		                      where kind = 'issue' and id = i.key
		                      order by event_seq desc limit 1) v on true
		 where i.project_key = $1
		 order by i.number`, fixture.Project)
	if err != nil {
		t.Fatalf("read each issue's latest keyword version: %v", err)
	}
	var read int
	var wrong []string
	for rows.Next() {
		var key, title string
		var body *string
		if err := rows.Scan(&key, &title, &body); err != nil {
			rows.Close()
			t.Fatalf("read each issue's latest keyword version: %v", err)
		}
		read++
		switch {
		case body == nil:
			wrong = append(wrong, key+" has no keyword version")
		case *body != title:
			wrong = append(wrong, fmt.Sprintf("%s's latest keyword version holds %q, want its title %q", key, *body, title))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read each issue's latest keyword version: %v", err)
	}
	if read != len(fixture.Keys) {
		t.Errorf("read %d issues of %s, want the %d the rig wrote", read, fixture.Project, len(fixture.Keys))
	}
	if len(wrong) > 0 {
		t.Errorf("%d issues' keyword versions do not hold their titles:\n\t%v", len(wrong), wrong)
	}

	var commentVersions, askVersions int64
	if err := database.Pool.QueryRow(t.Context(), `
		select count(*) filter (where kind = 'comment'), count(*) filter (where kind = 'ask')
		  from search_text_versions`).Scan(&commentVersions, &askVersions); err != nil {
		t.Fatalf("count the comments' and asks' keyword versions: %v", err)
	}
	if commentVersions != comments || askVersions != asks {
		t.Errorf("the keyword table holds %d comment versions and %d ask versions, want one of each of the %d comments and %d asks",
			commentVersions, askVersions, comments, asks)
	}
	t.Logf("the fill decided all %d events: %d issues with their titles, %d comments and %d asks", newest, read, comments, asks)
}

// TestASeedLeavesNoStoredKeywordReachAboveItsEvents: a kit loads this history through evalfixture
// into a database that a server serves, and that server's keyword fill has stored a reach. The
// reach says that every event created at or before it has its keyword rows. The history's events
// are dated in the past, below that reach, so the fill would write each of them as a breach of it.
// The seed nulls the stored reach in the statement that writes its events. The fill then runs
// over them with no refusal, and its cursor reaches the newest event.
func TestASeedLeavesNoStoredKeywordReachAboveItsEvents(t *testing.T) {
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(t.Context(), `
		insert into search_text_versions_fill (id, event_id, reach, rules) values (1, 0, now(), $1)`,
		textversions.RulesVersion); err != nil {
		t.Fatalf("store a reach over the empty log, as a serving fill does: %v", err)
	}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	fixture := Seed(t, database.Pool, Spec{Project: "LEGION", Trees: 2, Now: now})

	var reach *time.Time
	if err := database.Pool.QueryRow(t.Context(), `select reach from search_text_versions_fill where id = 1`).Scan(&reach); err != nil {
		t.Fatalf("read the keyword fill's stored reach: %v", err)
	}
	if reach != nil {
		t.Fatalf("after the seed the keyword fill's stored reach is %s, above the seeded events of %s to %s; want it nulled",
			reach.UTC().Format(time.RFC3339), fixture.From.Format(time.RFC3339), fixture.To.Format(time.RFC3339))
	}
	if err := textversions.FillToHorizon(t.Context(), database, events.NewHorizon(database.Pool)); err != nil {
		t.Fatalf("the keyword fill refused the seeded history: %v", err)
	}
	var newest int64
	if err := database.Pool.QueryRow(t.Context(), `select coalesce(max(id), 0) from events`).Scan(&newest); err != nil {
		t.Fatalf("read the newest event: %v", err)
	}
	if cursor := textversionstest.Cursor(t, database); newest == 0 || cursor != newest {
		t.Fatalf("the keyword fill's cursor stands at event %d, want the newest event %d", cursor, newest)
	}
}
