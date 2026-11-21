package eval

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// TestTheSeededStoreHoldsTheSeed writes the committed seed into an empty database and reads it
// back as Dispatch stores it: each issue's fields, its close and its claim at the seed's own times,
// its spec, each blocks and relates link, each ask with its answer and its opening event, each
// comment with its event, each message and file, and the project counters a server's next write
// takes. A second write leaves the store as it is, and a database holding another project is
// refused.
func TestTheSeededStoreHoldsTheSeed(t *testing.T) {
	ctx := context.Background()
	seed, err := LoadSeed(Files)
	if err != nil {
		t.Fatal(err)
	}
	database := storetest.Open(t)
	written, err := WriteSeed(ctx, database, seed)
	if err != nil || !written {
		t.Fatalf("WriteSeed on an empty database = %v, %v; want the seed written", written, err)
	}

	for _, want := range seed.Issues {
		var title, status, route, claimedBy string
		var labels []string
		var priority *int16
		var parent, assignee *string
		var created time.Time
		var closed, claimed *time.Time
		var specVersions int
		if err := database.Pool.QueryRow(ctx, `
			select i.title, i.status, i.labels, i.priority, i.parent_key, i.assignee, coalesce(i.route, ''),
			       i.created_at, i.closed_at, coalesce(i.claimed_by->>'id', ''), i.claimed_at,
			       (select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id
			         where a.issue_key = i.key and a.is_primary)
			from issues i where i.key = $1
		`, want.Key).Scan(&title, &status, &labels, &priority, &parent, &assignee, &route, &created, &closed,
			&claimedBy, &claimed, &specVersions); err != nil {
			t.Fatalf("read issue %s: %v", want.Key, err)
		}
		if title != want.Title || status != want.Status || !slices.Equal(labels, orEmpty(want.Labels)) {
			t.Errorf("issue %s holds %q %s %v, want %q %s %v", want.Key, title, status, labels, want.Title, want.Status, want.Labels)
		}
		if (priority == nil) != (want.Priority == nil) || (priority != nil && int(*priority) != *want.Priority) {
			t.Errorf("issue %s holds the priority %v, want %v", want.Key, priority, want.Priority)
		}
		if value(parent) != want.Parent || value(assignee) != want.Assignee || route != sessionRoute(want.Route) {
			t.Errorf("issue %s holds the parent %q, assignee %q and route %q, want %q, %q and %q",
				want.Key, value(parent), value(assignee), route, want.Parent, want.Assignee, sessionRoute(want.Route))
		}
		if !created.Equal(want.CreatedAt) || !sameTime(closed, want.ClosedAt) {
			t.Errorf("issue %s was created at %s and closed at %v, want %s and %v", want.Key, created, closed, want.CreatedAt, want.ClosedAt)
		}
		var wantClaim string
		var wantClaimed *time.Time
		if want.Claim != nil {
			wantClaim, wantClaimed = want.Claim.Session, &want.Claim.At
		}
		if claimedBy != wantClaim || !sameTime(claimed, wantClaimed) {
			t.Errorf("issue %s is claimed by %q at %v, want %q at %v", want.Key, claimedBy, claimed, wantClaim, wantClaimed)
		}
		if wantVersions := min(len(want.Spec), 1); specVersions != wantVersions {
			t.Errorf("the spec of issue %s holds %d versions, want %d", want.Key, specVersions, wantVersions)
		}
	}

	for _, link := range seed.Links {
		var held bool
		query := `select exists (select 1 from issue_links where issue_key = $2 and kind = 'blocked_by' and target_key = $1)`
		if link.Kind == LinkRelates {
			query = `select exists (select 1 from refs r join comments c on c.id::text = r.from_id
			          where r.from_kind = 'comment' and c.issue_key = $1 and r.to_kind = 'issue' and r.to_id = $2)`
		}
		if err := database.Pool.QueryRow(ctx, query, link.From, link.To).Scan(&held); err != nil {
			t.Fatalf("read the link from %s to %s: %v", link.From, link.To, err)
		}
		if !held {
			t.Errorf("the store holds no %s link from %s to %s", link.Kind, link.From, link.To)
		}
	}

	for _, ask := range seed.Asks {
		var state, author string
		var answer []byte
		var created time.Time
		var openedSeq *int
		var ownerSeq int
		if err := database.Pool.QueryRow(ctx, `
			select a.state, a.author->>'id', a.answer, a.created_at,
			       (select e.seq from events e where e.type = 'ask.opened' and e.payload->>'id' = a.id::text
			         and e.issue_key is not distinct from a.issue_key and e.artifact_id is not distinct from a.artifact_id),
			       coalesce((select last_seq from issues where key = a.issue_key), (select last_seq from artifacts where id = a.artifact_id))
			from asks a where a.id = $1
		`, ItemID("ask", ask.ID)).Scan(&state, &author, &answer, &created, &openedSeq, &ownerSeq); err != nil {
			t.Fatalf("read ask %s: %v", ask.ID, err)
		}
		if state != ask.State || author != ask.AskedBy || !created.Equal(ask.CreatedAt) {
			t.Errorf("ask %s is %s by %s at %s, want %s by %s at %s", ask.ID, state, author, created, ask.State, ask.AskedBy, ask.CreatedAt)
		}
		if ask.Answer != nil {
			var stored struct {
				User     string    `json:"user"`
				Selected []string  `json:"selected"`
				Text     string    `json:"text"`
				At       time.Time `json:"at"`
			}
			if err := json.Unmarshal(answer, &stored); err != nil || stored.User != ask.Answer.By ||
				!slices.Equal(stored.Selected, ask.Answer.Selected) || stored.Text != ask.Answer.Text || !stored.At.Equal(ask.Answer.At) {
				t.Errorf("ask %s holds the answer %s, want %+v", ask.ID, answer, *ask.Answer)
			}
		}
		if openedSeq == nil || *openedSeq > ownerSeq {
			t.Errorf("ask %s has the opening event %v under an owner whose last sequence is %d, want one at or below it", ask.ID, openedSeq, ownerSeq)
		}
	}

	for _, comment := range seed.Comments {
		var body, author string
		var created time.Time
		var eventBody *string
		var eventSeq *int
		var ownerSeq int
		if err := database.Pool.QueryRow(ctx, `
			select c.body, c.author->>'id', c.created_at, e.payload->>'body', e.seq,
			       coalesce((select last_seq from issues where key = c.issue_key), (select last_seq from artifacts where id = c.artifact_id))
			from comments c
			left join events e on e.type = 'comment.created' and e.payload->>'id' = c.id::text
			  and e.issue_key is not distinct from c.issue_key and e.artifact_id is not distinct from c.artifact_id
			where c.id = $1
		`, ItemID("comment", comment.ID)).Scan(&body, &author, &created, &eventBody, &eventSeq, &ownerSeq); err != nil {
			t.Fatalf("read comment %s: %v", comment.ID, err)
		}
		if body != comment.Body || author != comment.Author || !created.Equal(comment.CreatedAt) {
			t.Errorf("comment %s holds %q by %s at %s, want %q by %s at %s", comment.ID, body, author, created, comment.Body, comment.Author, comment.CreatedAt)
		}
		// An issue's log lists its comments as the comment route leaves them: each under a
		// comment.created event of its owner's, which carries its body.
		if eventSeq == nil || *eventSeq > ownerSeq || eventBody == nil || *eventBody != comment.Body {
			t.Errorf("comment %s has the comment.created event %v with the body %v under an owner whose last sequence is %d, want one at or below it with the comment's body",
				comment.ID, eventSeq, eventBody, ownerSeq)
		}
	}
	for _, message := range seed.Messages {
		var issue, author string
		var created time.Time
		var eventBody *string
		var eventAt *time.Time
		if err := database.Pool.QueryRow(ctx, `
			select m.issue_key, m.author->>'id', m.created_at, e.payload->>'body', e.created_at
			from messages m
			left join events e on e.type = 'message.created' and e.payload->>'id' = m.id::text and e.issue_key = m.issue_key
			where m.id = $1
		`, ItemID("message", message.ID)).Scan(&issue, &author, &created, &eventBody, &eventAt); err != nil {
			t.Fatalf("read message %s: %v", message.ID, err)
		}
		if issue != message.Issue || author != message.Author || !created.Equal(message.CreatedAt) {
			t.Errorf("message %s is on %s by %s at %s, want %s by %s at %s", message.ID, issue, author, created, message.Issue, message.Author, message.CreatedAt)
		}
		// An issue's log lists its messages as the message route leaves them: each under a
		// message.created event of its issue, dated when it was written, which carries its body.
		if eventBody == nil || *eventBody != message.Body || eventAt == nil || !eventAt.Equal(message.CreatedAt) {
			t.Errorf("message %s has the message.created event with the body %v at %v, want one on %s with the message's body at %s",
				message.ID, eventBody, eventAt, message.Issue, message.CreatedAt)
		}
	}
	// Each issue's, ask's and document's log holds the events its history appended, each dated
	// when it happened and carrying what its route's payload carries: an issue as it was created
	// (a done one todo, with no close and no claim), its claim, its close, an ask's answer, and a
	// document's or a file's create.
	type wantEvent struct {
		owner, kind string
		at          time.Time
		holds       map[string]any
	}
	var wants []wantEvent
	for _, issue := range seed.Issues {
		status := issue.Status
		if issue.ClosedAt != nil {
			status = "todo"
		}
		wants = append(wants, wantEvent{issue.Key, "issue.created", issue.CreatedAt,
			map[string]any{"key": issue.Key, "title": issue.Title, "status": status, "closed_at": nil, "claim": nil}})
		if issue.Claim != nil {
			wants = append(wants, wantEvent{issue.Key, "issue.claimed", issue.Claim.At,
				map[string]any{"key": issue.Key, "reason": "claimed", "claim": map[string]any{"actor": map[string]any{"id": issue.Claim.Session}}}})
		}
		if issue.ClosedAt != nil {
			wants = append(wants, wantEvent{issue.Key, "issue.closed", *issue.ClosedAt,
				map[string]any{"key": issue.Key, "status": "done", "changed": []string{"status", "closed_at"}}})
		}
	}
	for _, ask := range seed.Asks {
		if ask.Answer != nil {
			wants = append(wants, wantEvent{ask.Owner, "ask.answered", ask.Answer.At,
				map[string]any{"id": ItemID("ask", ask.ID), "state": "answered", "answer": map[string]any{"user": ask.Answer.By}}})
		}
	}
	for _, artifact := range seed.Artifacts {
		wants = append(wants, wantEvent{artifact.Owner + "/artifact/" + artifact.Slug, "artifact.created", artifact.CreatedAt,
			map[string]any{"artifact": map[string]any{"id": artifactID(artifact.Owner, artifact.Slug), "slug": artifact.Slug}}})
	}
	for _, want := range wants {
		holds, err := json.Marshal(want.holds)
		if err != nil {
			t.Fatal(err)
		}
		var found int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from events
			where type = $1 and created_at = $2 and payload @> $3::jsonb
			  and (payload->>'last_seq' is null or (payload->>'last_seq')::int = seq)
		`, want.kind, want.at, string(holds)).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if found != 1 {
			t.Errorf("%s has %d %s events at %s holding %s, want one", want.owner, found, want.kind, want.at, holds)
		}
	}
	// Each owner's log is in the order its events happened, as a server writing them one by one
	// numbers them: no event is numbered after a later one.
	var disordered []string
	rows, err := database.Pool.Query(ctx, `
		select coalesce(issue_key, artifact_id::text) || ' ' || seq || ' ' || type
		from (select *, lag(created_at) over (partition by issue_key, artifact_id order by seq) as before from events) e
		where before > created_at order by 1
	`)
	if err != nil {
		t.Fatal(err)
	}
	if disordered, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if len(disordered) > 0 {
		t.Errorf("these events are numbered after a later event of their owner: %v", disordered)
	}
	for _, artifact := range seed.Artifacts {
		var stored string
		if err := database.Pool.QueryRow(ctx, `
			select coalesce(convert_from(v.content, 'UTF8'), v.markdown) from artifact_versions v
			join artifacts a on a.id = v.artifact_id where a.slug = $1 and v.number = 1
		`, artifact.Slug).Scan(&stored); err != nil {
			t.Fatalf("read %s/%s: %v", artifact.Owner, artifact.Slug, err)
		}
		if artifact.Kind == ArtifactFile && stored != artifact.Body {
			t.Errorf("the file %s holds %q, want %q", artifact.Slug, stored, artifact.Body)
		}
		if first := strings.SplitN(artifact.Body, "\n", 2)[0]; artifact.Kind == ArtifactDocument && !strings.Contains(stored, strings.TrimPrefix(first, "# ")) {
			t.Errorf("the document %s holds %q, which lacks its first line %q", artifact.Slug, stored, first)
		}
	}

	// A server reads a document's text from its live state: each seeded document and spec
	// answers its own text through the document service, as the text routes read it.
	service := docs.New(docs.Deps{Store: database, Events: events.NewBroker()})
	defer func() { _ = service.Shutdown(ctx) }()
	read := func(owner, slug, markdown string) {
		t.Helper()
		text, _, err := service.TextWithToken(ctx, artifactID(owner, slug))
		first := strings.TrimPrefix(strings.SplitN(markdown, "\n", 2)[0], "# ")
		if err != nil || !strings.Contains(text, first) {
			t.Errorf("the text of %s/%s reads %q (%v), which lacks its first line %q", owner, slug, text, err, first)
		}
	}
	for _, artifact := range seed.Artifacts {
		if artifact.Kind == ArtifactDocument {
			read(artifact.Owner, artifact.Slug, artifact.Body)
		}
	}
	for _, issue := range seed.Issues {
		if issue.Spec != "" {
			read(issue.Key, "spec", issue.Spec)
		}
	}

	var unnumbered int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from projects p where p.next_number <> (select max(number) + 1 from issues where project_key = p.key)
	`).Scan(&unnumbered); err != nil || unnumbered != 0 {
		t.Errorf("%d projects number their next issue at another number than one past their last (%v)", unnumbered, err)
	}

	again, err := WriteSeed(ctx, database, seed)
	if err != nil || again {
		t.Errorf("WriteSeed on the seeded database = %v, %v; want nothing written and no error", again, err)
	}
	var issues int
	if err := database.Pool.QueryRow(ctx, `select count(*) from issues`).Scan(&issues); err != nil || issues != len(seed.Issues) {
		t.Errorf("the store holds %d issues after a second write, want %d (%v)", issues, len(seed.Issues), err)
	}

	other := storetest.Open(t)
	for _, statement := range []string{
		`insert into people (email) values ('x@example.com')`,
		`insert into projects (key, name, default_assignee) values ('ELSE', 'Something else', 'x@example.com')`,
	} {
		if _, err := other.Pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if written, err := WriteSeed(ctx, other, seed); written || err == nil || !strings.Contains(err.Error(), "ELSE") {
		t.Errorf("WriteSeed on a database holding project ELSE = %v, %v; want a refusal naming ELSE", written, err)
	}
}

func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func value(pointer *string) string {
	if pointer == nil {
		return ""
	}
	return *pointer
}

func sameTime(got, want *time.Time) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return got.Equal(*want)
}
