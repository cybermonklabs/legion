package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// keywordItem is one item of the keyword table: its kind and its id, an issue's key or a post's or
// an ask's uuid.
type keywordItem struct{ kind, id string }

// keptItem is an item as a kept time read it: the text its search column is made from (an issue's
// title, a post's body, or an ask's question, a space and ask_search_suffix of its options and
// answer), the column as text, its issue, and for an issue its primary document and that
// document's latest version.
type keptItem struct {
	text, search string
	issueKey     *string
	spec         *string
	specVersion  *int32
}

// keptTime is a database time at which nothing else was written, and every item as it stood then.
type keptTime struct {
	step  string
	at    time.Time
	items map[keywordItem]keptItem
	// inside is the kept time inside the reversed pair's transaction A, which was open then.
	inside bool
}

// keywordVersion is one row the relation answered at a kept time, with the columns of the event
// that started it.
type keywordVersion struct {
	issueKey     *string
	projectKey   string
	eventID      int64
	eventSeq     int32
	startedAt    time.Time
	body         string
	bodyFrom     string
	specArtifact *string
	specVersion  *int32
	search       string
	// deletedAt is the relation's deleted_at: the post's own delete mark, whatever the time.
	deletedAt *time.Time
	// The event's: its type, seq, created_at, issue and project as the log names them, the item it
	// is of (an issue event's issue, else its payload's id), and whether it cut the post's body.
	// eventType is "" for an event the log does not hold.
	eventType    string
	eventSeqOf   int32
	eventAt      time.Time
	eventIssue   *string
	eventProject string
	eventItem    string
	truncated    bool
}

// keywordHistory is the kept times of a test that writes items through the API, and what the
// relation answered at each once the fill ran.
type keywordHistory struct {
	t        *testing.T
	database *store.Store
	kept     []keptTime
	versions []map[keywordItem][]keywordVersion
	// reversed is the reversed pair's comment, and reversedEvent transaction A's event of it.
	reversed      keywordItem
	reversedEvent int64
}

// keep keeps a database time after step at which no other transaction of the database is open,
// and every item's search column and text as they stand then (keywordHistory.read).
func (h *keywordHistory) keep(step string) {
	h.t.Helper()
	h.keepAt(step, nil)
}

// keepInside keeps a time as keep does while the transaction of backend is open, inside its
// window, which no other kept time falls in.
func (h *keywordHistory) keepInside(step string, backend int32) {
	h.t.Helper()
	h.keepAt(step, []int32{backend})
}

// keepAt keeps a time as keep does while the transactions of the backends open may be open: a
// time inside their window when open names any.
func (h *keywordHistory) keepAt(step string, open []int32) {
	h.t.Helper()
	var kept keptTime
	testwait.WithinPolling(h.t, 30*time.Second, 20*time.Millisecond,
		fmt.Sprintf("a moment within 30 s after %s at which no other transaction of the database is open", step), func() bool {
			var quiet bool
			kept, quiet = h.read(open)
			return quiet
		})
	kept.step, kept.inside = step, len(open) > 0
	h.kept = append(h.kept, kept)
}

// keywordBusyQuery answers how many events are committed, and whether another client backend of
// this database is in a transaction, other than those of $1.
const keywordBusyQuery = `
	select (select count(*) from events),
	       exists (select from pg_stat_activity a
	                where a.datname = current_database() and a.pid <> pg_backend_pid()
	                  and a.backend_type = 'client backend' and a.xact_start is not null
	                  and a.pid <> all($1::int[]))`

// keywordItemsQuery reads every item and the text its search column is made from, the column
// itself, its issue, and an issue's primary document and that document's latest version.
const keywordItemsQuery = `
	select 'issue', i.key, i.title, i.search::text, i.key, a.id::text,
	       (select max(v.number) from artifact_versions v where v.artifact_id = a.id)
	  from issues i left join artifacts a on a.issue_key = i.key and a.is_primary
	union all
	select 'comment', c.id::text, c.body, c.search::text, c.issue_key, null, null from comments c
	union all
	select 'message', m.id::text, m.body, m.search::text, m.issue_key, null, null from messages m
	union all
	select 'ask', k.id::text, k.question || ' ' || ask_search_suffix(k.options, k.answer), k.search::text,
	       k.issue_key, null, null
	  from asks k`

// read reads a kept time and the items at it, and answers whether the moment was quiet. The time
// is read with the count of the events committed then; then no other transaction may be open (but
// those of open), so each transaction that began before the time has committed; then the items are
// read in one snapshot; then again no other transaction may be open, and the count of events must
// not have changed since the time was read, so no write that began after the time is among what
// the snapshot read.
func (h *keywordHistory) read(open []int32) (keptTime, bool) {
	h.t.Helper()
	ctx := h.t.Context()
	if open == nil {
		open = []int32{}
	}
	busy := func() (int64, bool) {
		var count int64
		var busy bool
		if err := h.database.Pool.QueryRow(ctx, keywordBusyQuery, open).Scan(&count, &busy); err != nil {
			h.t.Fatalf("read the database's open transactions: %v", err)
		}
		return count, busy
	}
	var kept keptTime
	var at int64
	if err := h.database.Pool.QueryRow(ctx, `select clock_timestamp(), (select count(*) from events)`).Scan(&kept.at, &at); err != nil {
		h.t.Fatalf("read the database's time: %v", err)
	}
	before, open1 := busy()
	if open1 || before != at {
		return keptTime{}, false
	}
	tx, err := h.database.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		h.t.Fatalf("begin the read of the items: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var during int64
	if err := tx.QueryRow(ctx, `select count(*) from events`).Scan(&during); err != nil {
		h.t.Fatalf("count the events: %v", err)
	}
	rows, err := tx.Query(ctx, keywordItemsQuery)
	if err != nil {
		h.t.Fatalf("read the items: %v", err)
	}
	kept.items = map[keywordItem]keptItem{}
	for rows.Next() {
		var key keywordItem
		var item keptItem
		if err := rows.Scan(&key.kind, &key.id, &item.text, &item.search, &item.issueKey, &item.spec, &item.specVersion); err != nil {
			h.t.Fatalf("read the items: %v", err)
		}
		kept.items[key] = item
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("read the items: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		h.t.Fatalf("end the read of the items: %v", err)
	}
	after, open2 := busy()
	return kept, !open2 && before == during && during == after
}

// keywordVersionsQuery reads the relation at a time, whose SQL stands in for {relation}, joined
// with the table's own row of each version, for its body_from, and with the event that started it.
const keywordVersionsQuery = `
	select v.kind, v.id, v.issue_key, v.project_key, v.event_id, v.event_seq, v.started_at, v.body, t.body_from,
	       v.spec_artifact_id::text, v.spec_version, v.search::text, v.deleted_at,
	       coalesce(e.type, ''), coalesce(e.seq, -1), coalesce(e.created_at, 'epoch'::timestamptz), e.issue_key,
	       coalesce(e.project_key, ei.project_key, ea.project_key, ''),
	       coalesce(case when e.type like 'issue.%' or e.type = 'artifact.version' then e.issue_key
	                     else e.payload->>'id' end, ''),
	       coalesce(e.payload->>'body_truncated' = 'true', false)
	  from {relation} v
	  join search_text_versions t on t.kind = v.kind and t.id = v.id and t.event_seq = v.event_seq
	  left join events e on e.id = v.event_id
	  left join issues ei on ei.key = e.issue_key
	  left join artifacts ea on ea.id = e.artifact_id`

// verify reads the relation at each kept time through store.TextVersionsAt, once the fill has
// reached the last of them, and holds each item that existed then to it (keywordHistory.check).
// At the last kept time each item's version is also its last row.
func (h *keywordHistory) verify() {
	h.t.Helper()
	ctx := h.t.Context()
	keywordAwaitReach(h.t, h.database, h.kept[len(h.kept)-1].at)
	h.versions = make([]map[keywordItem][]keywordVersion, len(h.kept))
	for index, kept := range h.kept {
		relation, err := h.database.TextVersionsAt(ctx, kept.at)
		if err != nil {
			h.t.Fatalf("TextVersionsAt %s (kept after %s): %v", kept.at.Format(time.RFC3339Nano), kept.step, err)
		}
		rows, err := h.database.Pool.Query(ctx, strings.Replace(keywordVersionsQuery, "{relation}", relation.SQL(), 1))
		if err != nil {
			h.t.Fatalf("read the relation at the time kept after %s: %v", kept.step, err)
		}
		got := map[keywordItem][]keywordVersion{}
		for rows.Next() {
			var key keywordItem
			var v keywordVersion
			if err := rows.Scan(&key.kind, &key.id, &v.issueKey, &v.projectKey, &v.eventID, &v.eventSeq, &v.startedAt, &v.body,
				&v.bodyFrom, &v.specArtifact, &v.specVersion, &v.search, &v.deletedAt, &v.eventType, &v.eventSeqOf, &v.eventAt, &v.eventIssue,
				&v.eventProject, &v.eventItem, &v.truncated); err != nil {
				h.t.Fatalf("read the relation at the time kept after %s: %v", kept.step, err)
			}
			got[key] = append(got[key], v)
		}
		if err := rows.Err(); err != nil {
			h.t.Fatalf("read the relation at the time kept after %s: %v", kept.step, err)
		}
		h.versions[index] = got
	}
	failures := 0
	fail := func(format string, args ...any) {
		h.t.Helper()
		if failures++; failures <= 25 {
			h.t.Errorf(format, args...)
		}
	}
	later := h.laterWords()
	marks := h.deleteMarks()
	for index, kept := range h.kept {
		for key, got := range h.versions[index] {
			if _, existed := kept.items[key]; !existed {
				fail("after %s: the relation holds a version of %s %s, which did not exist then", kept.step, key.kind, key.id)
			}
			for _, v := range got {
				if want := marks[key]; !keywordSameTime(v.deletedAt, want) {
					fail("after %s: %s %s's version (event %d) answers deleted_at %v; the item's own delete mark is %v",
						kept.step, key.kind, key.id, v.eventID, keywordValue(v.deletedAt), keywordValue(want))
				}
			}
		}
		for key, item := range kept.items {
			h.check(fail, index, key, item, later[key][index])
		}
	}
	var directRows int
	if err := h.database.Pool.QueryRow(ctx, `
		select count(*) from search_text_versions v join messages m on m.id::text = v.id
		 where v.kind = 'message' and m.issue_key is null`).Scan(&directRows); err != nil {
		h.t.Fatalf("count the rows of the messages with no issue: %v", err)
	}
	if directRows != 0 {
		fail("the keyword table holds %d rows of messages with no issue, want none", directRows)
	}
	if failures > 25 {
		h.t.Errorf("... and %d more failures", failures-25)
	}
}

// check holds the versions of key at kept time index to the item as it stood then: one version;
// its vector the kept column; its body the kept text (word for word for a body read from a
// comment's blocks, which keep the words of the body and not the white space between its blocks)
// and none of the words written into the item only after the time, later; an issue's naming the
// primary document and the version of it the issue held; and its issue, project, seq and start
// its event's, an event of the item. A message with no issue has no version. Inside the reversed
// pair's window its comment's version is transaction A's, whose text was written after the time,
// and the controls on the vector and the text do not apply; after A's commit its version is A's.
func (h *keywordHistory) check(fail func(string, ...any), index int, key keywordItem, item keptItem, later []string) {
	kept := h.kept[index]
	got := h.versions[index][key]
	if key.kind == "message" && item.issueKey == nil {
		if len(got) != 0 {
			fail("after %s: the message with no issue %s has %d versions, want none", kept.step, key.id, len(got))
		}
		return
	}
	if len(got) != 1 {
		fail("after %s: %s %s has %d versions at %s, want one", kept.step, key.kind, key.id, len(got), kept.at.Format(time.RFC3339Nano))
		return
	}
	v := got[0]
	if key == h.reversed && (kept.inside || index > slices.IndexFunc(h.kept, func(k keptTime) bool { return k.inside })) {
		if v.eventID != h.reversedEvent {
			fail("after %s: the reversed pair's comment has the version of event %d (seq %d), want transaction A's event %d",
				kept.step, v.eventID, v.eventSeq, h.reversedEvent)
		}
	}
	if !(kept.inside && key == h.reversed) {
		if v.search != item.search {
			fail("after %s: %s %s's version (event %d) has the vector %.200s; its search column was %.200s",
				kept.step, key.kind, key.id, v.eventID, v.search, item.search)
		}
		if !keywordSameBody(v, item.text) {
			fail("after %s: %s %s's version (event %d, body from %s) holds %q; its text was %q",
				kept.step, key.kind, key.id, v.eventID, v.bodyFrom, keywordExcerpt(v.body), keywordExcerpt(item.text))
		}
		words := keywordWords(v.body)
		for _, word := range later {
			if words[word] {
				fail("after %s: %s %s's version (event %d) holds %q, written after that time", kept.step, key.kind, key.id, v.eventID, word)
				break
			}
		}
	}
	if key.kind == "issue" && (!keywordSame(v.specArtifact, item.spec) || !keywordSame(v.specVersion, item.specVersion)) {
		fail("after %s: issue %s's version (event %d) names document %v version %v; the issue held %v version %v",
			kept.step, key.id, v.eventID, keywordValue(v.specArtifact), keywordValue(v.specVersion), keywordValue(item.spec), keywordValue(item.specVersion))
	}
	switch {
	case v.eventType == "":
		fail("after %s: %s %s's version names event %d, which the log does not hold", kept.step, key.kind, key.id, v.eventID)
	case v.eventItem != key.id:
		fail("after %s: %s %s's version names event %d (%s), an event of %s", kept.step, key.kind, key.id, v.eventID, v.eventType, v.eventItem)
	case v.eventSeqOf != v.eventSeq || !v.eventAt.Equal(v.startedAt) || !keywordSame(v.eventIssue, v.issueKey) || v.eventProject != v.projectKey:
		fail("after %s: %s %s's version holds seq %d, start %s, issue %v and project %q; its event %d (%s) has seq %d, created_at %s, issue %v and project %q",
			kept.step, key.kind, key.id, v.eventSeq, v.startedAt.Format(time.RFC3339Nano), keywordValue(v.issueKey), v.projectKey,
			v.eventID, v.eventType, v.eventSeqOf, v.eventAt.Format(time.RFC3339Nano), keywordValue(v.eventIssue), v.eventProject)
	}
}

// checkLast holds each item's version at the last kept time, after every change, to its last row:
// an item unchanged since its last version holds that version, whose vector and body check held to
// its present column and text.
func (h *keywordHistory) checkLast(fail func(string, ...any)) {
	rows, err := h.database.Pool.Query(h.t.Context(), `select kind, id, max(event_seq) from search_text_versions group by kind, id`)
	if err != nil {
		h.t.Fatalf("read each item's last row: %v", err)
	}
	lastSeq, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct {
		key keywordItem
		seq int32
	}, error) {
		var last struct {
			key keywordItem
			seq int32
		}
		err := row.Scan(&last.key.kind, &last.key.id, &last.seq)
		return last, err
	})
	if err != nil {
		h.t.Fatalf("read each item's last row: %v", err)
	}
	end := len(h.kept) - 1
	for _, last := range lastSeq {
		if got := h.versions[end][last.key]; len(got) == 1 && got[0].eventSeq != last.seq {
			fail("at the end: %s %s holds its version of seq %d, not its last row's seq %d", last.key.kind, last.key.id, got[0].eventSeq, last.seq)
		}
	}
}

// deleteMarks reads the delete mark of each deleted comment and message as it stands now, which
// the relation answers on every version of the post at every time (store.TextVersions).
func (h *keywordHistory) deleteMarks() map[keywordItem]*time.Time {
	h.t.Helper()
	rows, err := h.database.Pool.Query(h.t.Context(), `
		select 'comment', id::text, deleted_at from comments where deleted_at is not null
		union all
		select 'message', id::text, deleted_at from messages where deleted_at is not null`)
	if err != nil {
		h.t.Fatalf("read the posts' delete marks: %v", err)
	}
	defer rows.Close()
	marks := map[keywordItem]*time.Time{}
	for rows.Next() {
		var key keywordItem
		var at time.Time
		if err := rows.Scan(&key.kind, &key.id, &at); err != nil {
			h.t.Fatalf("read a post's delete mark: %v", err)
		}
		marks[key] = &at
	}
	if err := rows.Err(); err != nil {
		h.t.Fatalf("read the posts' delete marks: %v", err)
	}
	return marks
}

// keywordSameTime is whether a and b are both nil or name the same instant.
func keywordSameTime(a, b *time.Time) bool {
	return (a == nil) == (b == nil) && (a == nil || a.Equal(*b))
}

// laterWords answers, for each item and kept time, the words its texts at later kept times hold
// that none of its texts up to that time held: the text written after the time.
func (h *keywordHistory) laterWords() map[keywordItem]map[int][]string {
	out := map[keywordItem]map[int][]string{}
	items := map[keywordItem]bool{}
	for _, kept := range h.kept {
		for key := range kept.items {
			items[key] = true
		}
	}
	for key := range items {
		out[key] = map[int][]string{}
		for index := range h.kept {
			seen := map[string]bool{}
			for _, kept := range h.kept[:index+1] {
				if item, ok := kept.items[key]; ok {
					for word := range keywordWords(item.text) {
						seen[word] = true
					}
				}
			}
			added := map[string]bool{}
			for _, kept := range h.kept[index+1:] {
				if item, ok := kept.items[key]; ok {
					for word := range keywordWords(item.text) {
						if !seen[word] {
							added[word] = true
						}
					}
				}
			}
			for word := range added {
				out[key][index] = append(out[key][index], word)
			}
			slices.Sort(out[key][index])
		}
	}
	return out
}

// requireVersion fails unless the version of key at the time kept after step holds want.
func (h *keywordHistory) requireVersion(step string, key keywordItem, want func(keywordVersion) bool, wanted string) {
	h.t.Helper()
	index := slices.IndexFunc(h.kept, func(k keptTime) bool { return k.step == step })
	if index < 0 {
		h.t.Fatalf("no time was kept after %s", step)
	}
	got := h.versions[index][key]
	if len(got) != 1 || !want(got[0]) {
		h.t.Errorf("after %s: %s %s's versions are %+v, want one %s", step, key.kind, key.id, got, wanted)
	}
}

// keywordAwaitReach waits up to 30 s for the keyword table's reach to come to at, the latest time
// the test reads the relation at.
func keywordAwaitReach(t *testing.T, database *store.Store, at time.Time) {
	t.Helper()
	var reach time.Time
	var reached bool
	if !testwait.Holds(30*time.Second, 100*time.Millisecond, func() bool {
		var err error
		if reach, reached, err = database.TextVersionsReach(t.Context()); err != nil {
			t.Fatalf("read the keyword table's reach: %v", err)
		}
		return reached && !reach.Before(at.Truncate(time.Microsecond))
	}) {
		t.Fatalf("the keyword table's reach is %s (reached %v) 30 s on; the last kept time is %s",
			reach.Format(time.RFC3339Nano), reached, at.Format(time.RFC3339Nano))
	}
}

// keywordSameBody is whether v's body is text: the same words for a body read from a comment's
// first blocks, which drop the white space that ends each block, and the same string otherwise.
func keywordSameBody(v keywordVersion, text string) bool {
	if v.bodyFrom == "blocks" {
		return strings.Join(strings.Fields(v.body), " ") == strings.Join(strings.Fields(text), " ")
	}
	return v.body == text
}

// keywordWords is the set of words of text, lower case.
func keywordWords(text string) map[string]bool {
	words := map[string]bool{}
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		words[word] = true
	}
	return words
}

// keywordExcerpt is text cut for a failure message.
func keywordExcerpt(text string) string {
	if len(text) > 160 {
		return text[:160] + fmt.Sprintf("… (%d bytes)", len(text))
	}
	return text
}

// keywordSame is whether a and b are both nil or point to equal values.
func keywordSame[T comparable](a, b *T) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// keywordValue is the value value points to, for a failure message, and nil for none.
func keywordValue[T any](value *T) any {
	if value == nil {
		return nil
	}
	return *value
}
