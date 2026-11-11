package refs

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// What INFRA-988 holds scopeSourcesQuery's seed to: that its arms answer what the one `or`-ed
// seed before them answered, over a comment of every shape an arm can match, and that each arm
// is read through an index of comments rather than by a scan of the whole table. Those are two
// questions - one about the set, one about the plan - and each is asked here beside its own
// control, the statement as it read before the arms.

// scopeSourcesOneSeedQuery is scopeSourcesQuery as it read before the arms, and this file's
// control: one seed term whose four predicates are `or`-ed, the whole of them `and`-ed with the
// scope test, which is the shape no index serves. Everything outside the seed is the statement's
// own text, and the pieces the two share (askScope, commentOwnScope and their joins) are the
// definition of a scope rather than a shape of a read, so a difference between the two is a
// difference the arms made.
const scopeSourcesOneSeedQuery = `
	with recursive
	spec as (
		select issue_key from artifacts where id = $1 and is_primary
	),
	scoped_asks as (
		select k.id from asks k ` + askScopeJoin + `
		where (coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid) = $1
		       or k.artifact_id = $1
		       or k.issue_key in (select issue_key from spec))
		  and ` + askScope + ` = $1
	),
	scoped_comments(id) as (
		select c.id from comments c ` + commentOwnScopeJoin + `
		left join artifacts cs on cs.issue_key = c.issue_key and cs.is_primary
		where ((c.anchor->>'artifact_id')::uuid = $1
		       or c.artifact_id = $1
		       or c.ask_id in (select id from scoped_asks)
		       or c.issue_key in (select issue_key from spec))
		  and (` + commentOwnScope + ` = $1
		       or (` + commentOwnScope + ` is null and c.reply_to is null and cs.id = $1))
		union
		select c.id from scoped_comments p
		join comments c on c.reply_to = p.id ` + commentOwnScopeJoin + `
		where ` + commentOwnScope + ` is null
	)
	select kind, id::text from (
		select 'artifact' as kind, id from artifacts where id = $1
		union all
		select 'ask', id from scoped_asks
		union all
		select 'comment', id from scoped_comments
		union all
		select 'message', m.id from messages m where m.issue_key in (select issue_key from spec)
	) sources
	order by kind, id`

// scopeSourcesSeedArms names the arms of scopeSourcesQuery's seed, in the order the statement
// writes them, each by the index it is there to be read through. The tests below address an arm
// by its position: one drops it to show the fixture reaches it, the other asks the plan for its
// index.
var scopeSourcesSeedArms = []struct{ name, index string }{
	{"the comments anchored to the document", "comments_anchor_artifact"},
	{"the comments the document owns", "comments_artifact_id"},
	{"the replies to the document's asks", "comments_ask_id"},
	{"the comments of the issue the document is the spec of", "comments_issue_created"},
}

// seedArmSeparator is what joins the arms of the seed, and the recursive term after them.
const seedArmSeparator = "\n\t\tunion\n"

// scopeSourcesWithoutArm is scopeSourcesQuery with arm number n of its seed left out, which is
// how the equality below shows it can see a row go missing: an arm whose removal changes no
// document's sources is an arm the fixture never reached, and the equality would be proving
// nothing about it.
func scopeSourcesWithoutArm(t *testing.T, n int) string {
	t.Helper()
	const opening = "scoped_comments(id) as (\n"
	const closing = "\n\t)\n\tselect kind"
	start := strings.Index(scopeSourcesQuery, opening)
	end := strings.Index(scopeSourcesQuery, closing)
	if start < 0 || end <= start {
		t.Fatalf("scopeSourcesQuery no longer writes its walk as a scoped_comments term this test can read:\n%s", scopeSourcesQuery)
	}
	start += len(opening)
	arms := strings.Split(scopeSourcesQuery[start:end], seedArmSeparator)
	if len(arms) != len(scopeSourcesSeedArms)+1 {
		t.Fatalf("scopeSourcesQuery's walk has %d terms, want %d seed arms and the recursive term:\n%s",
			len(arms), len(scopeSourcesSeedArms)+1, scopeSourcesQuery)
	}
	kept := slices.Delete(slices.Clone(arms), n, n+1)
	return scopeSourcesQuery[:start] + strings.Join(kept, seedArmSeparator) + scopeSourcesQuery[end:]
}

// scopeSourcesBy is what statement answers for the document doc, as a set: both statements write
// the same `order by`, and sorting here holds them to answering the same rows rather than to
// answering them in the same order.
func scopeSourcesBy(t *testing.T, tx pgx.Tx, statement, doc string) []Target {
	t.Helper()
	sources, err := scanTargets(tx.Query(context.Background(), statement, doc))
	if err != nil {
		t.Fatalf("list the sources in the scope of %s: %v", doc, err)
	}
	slices.SortFunc(sources, func(a, b Target) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID))
	})
	return sources
}

// insertSeedIssue creates one issue of the fixtures below. createReferenceIssue writes number 1,
// and an issue's number is unique in its project (store/migrations/0001_init.up.sql:14), so a
// fixture of more than one issue numbers its own.
func insertSeedIssue(t *testing.T, tx pgx.Tx, key, project string, number int) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, $3, 'Seed issue', '{"kind":"user","id":"alice"}', lpad(($3::int)::text, 20, '0'))
	`, key, project, number); err != nil {
		t.Fatalf("create issue %q: %v", key, err)
	}
}

// seedDocument is one document the equality below reads the sources of, with a name a failure
// cites it by.
type seedDocument struct{ name, id string }

// One comment of every shape an arm of the seed can match, including comments two arms match,
// the replies below one of those, a cycle of replies the walk enters, a thread that loops before
// naming any document, and a document with no sources at all: for each document the arms answer
// exactly what the one `or`-ed seed answers, and each comment they answer is one ScopeOf puts in
// that document.
//
// Equal sets would mean nothing if the fixture reached only some of the arms, so each arm is
// dropped in turn and the sources must change. That is what says this test can see a difference
// at all.
func TestScopeSourcesSeedArmsAnswerTheOneSeed(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	createReferenceProject(t, tx, "ACME")
	createReferenceIssue(t, tx, "ACME-1", "ACME")
	insertSeedIssue(t, tx, "ACME-2", "ACME", 2)
	spec := insertScopeDocument(t, tx, "ACME-1", "spec", true)
	notes := insertScopeDocument(t, tx, "ACME-1", "notes", false)
	quiet := insertScopeDocument(t, tx, "ACME-1", "quiet", false)
	handbook, _ := createReferenceArtifact(t, tx, "", "ACME", "handbook")
	otherSpec := insertScopeDocument(t, tx, "ACME-2", "spec", true)

	blockAsk := insertScopeBlockAsk(t, tx, "ACME-1", notes, "q1")
	issueAsk := insertScopeAsk(t, tx, "ACME-1", "", nil)
	projectAsk := insertScopeAsk(t, tx, nil, "artifact_id", handbook)

	// One comment per arm, each reachable through that arm alone: the anchored one and the
	// reply to the block ask are in an issue whose spec is not the document they name, and the
	// document the owned one names has no issue at all.
	anchored := insertScopeRow(t, tx, "a comment anchored to notes", `
		insert into comments (issue_key, author, body, anchor)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'anchored', jsonb_build_object('artifact_id', $1::text, 'mark_id', 'm1'))
		returning id::text
	`, notes)
	owned := insertScopeRow(t, tx, "a comment the handbook owns", `
		insert into comments (artifact_id, author, body) values ($1, '{"kind":"user","id":"alice"}', 'owned')
		returning id::text
	`, handbook)
	insertScopeRow(t, tx, "a reply to the block ask", `
		insert into comments (issue_key, author, body, ask_id)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'reply', $1) returning id::text
	`, blockAsk)
	issueComment := createReferenceComment(t, tx, "ACME-1")

	// The comments two arms match: one anchored to the spec of the issue it is in (the anchor
	// arm and the issue arm), one the handbook owns replying to the handbook's ask (the owner
	// arm and the ask arm), and one replying to an ask of the issue (the ask arm and the issue
	// arm). `union` is what answers each of them once.
	twoArms := insertScopeRow(t, tx, "a comment anchored to its own issue's spec", `
		insert into comments (issue_key, author, body, anchor)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'two arms', jsonb_build_object('artifact_id', $1::text, 'mark_id', 'm2'))
		returning id::text
	`, spec)
	insertScopeRow(t, tx, "a reply to the handbook's ask the handbook owns", `
		insert into comments (artifact_id, author, body, ask_id)
		values ($1, '{"kind":"user","id":"alice"}', 'owned reply', $2) returning id::text
	`, handbook, projectAsk)
	insertScopeRow(t, tx, "a reply to the issue's ask", `
		insert into comments (issue_key, author, body, ask_id)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'reply', $1) returning id::text
	`, issueAsk)

	// The walk down the replies, from a root two arms match: every comment under twoArms is
	// reached by each of those two arms' derivations and must be answered once all the same.
	// The chain is three deep, so a seed answered twice would double every level of it.
	replies := []string{twoArms}
	for depth := 1; depth <= 3; depth++ {
		replies = append(replies, insertScopeRow(t, tx, fmt.Sprintf("a reply %d deep under the two-arm comment", depth), `
			insert into comments (issue_key, author, body, reply_to)
			values ('ACME-1', '{"kind":"user","id":"alice"}', 'reply', $1) returning id::text
		`, replies[depth-1]))
	}
	anchoredReply := insertScopeRow(t, tx, "a reply to the anchored comment", `
		insert into comments (issue_key, author, body, reply_to)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'reply', $1) returning id::text
	`, anchored)
	insertScopeRow(t, tx, "a reply to that reply", `
		insert into comments (issue_key, author, body, reply_to)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'reply', $1) returning id::text
	`, anchoredReply)
	insertScopeRow(t, tx, "a reply to the issue comment", `
		insert into comments (issue_key, author, body, reply_to)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'reply', $1) returning id::text
	`, issueComment)
	insertScopeRow(t, tx, "a reply the handbook owns", `
		insert into comments (artifact_id, author, body, reply_to)
		values ($1, '{"kind":"user","id":"alice"}', 'owned reply', $2) returning id::text
	`, handbook, owned)

	// A cycle the walk enters: the head is a seed row through its anchor, its reply names no
	// document and is walked to, and that reply is the head's own parent. The walk reaches the
	// head a second time and stops there, because a recursive `union` drops a row already in
	// the result rather than recursing on it again.
	cycleHead := insertScopeRow(t, tx, "the head of a reply cycle", `
		insert into comments (issue_key, author, body, anchor)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'cycle head', jsonb_build_object('artifact_id', $1::text, 'mark_id', 'm3'))
		returning id::text
	`, spec)
	cycleTail := insertScopeRow(t, tx, "the tail of a reply cycle", `
		insert into comments (issue_key, author, body, reply_to)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'cycle tail', $1) returning id::text
	`, cycleHead)
	if _, err := tx.Exec(ctx, `update comments set reply_to = $2 where id = $1`, cycleHead, cycleTail); err != nil {
		t.Fatalf("close the reply cycle: %v", err)
	}

	// A thread of replies naming no document that loops before any of them names one: ScopeOf
	// answers none for each, and neither statement reaches them.
	loose := insertScopeRow(t, tx, "a reply that will be looped", `
		insert into comments (issue_key, author, body) values ('ACME-1', '{"kind":"user","id":"alice"}', 'loose')
		returning id::text
	`)
	looping := insertScopeRow(t, tx, "a reply looping back on it", `
		insert into comments (issue_key, author, body, reply_to)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'loop', $1) returning id::text
	`, loose)
	if _, err := tx.Exec(ctx, `update comments set reply_to = $2 where id = $1`, loose, looping); err != nil {
		t.Fatalf("close the loose reply cycle: %v", err)
	}

	// The other issue's own comment, which belongs to its own spec and to no document of this
	// one, and the messages both statements read the same way.
	insertScopeRow(t, tx, "the other issue's comment", `
		insert into comments (issue_key, author, body) values ('ACME-2', '{"kind":"user","id":"alice"}', 'other')
		returning id::text
	`)
	insertScopeRow(t, tx, "an issue message", `
		insert into messages (issue_key, author, body) values ('ACME-1', '{"kind":"session","id":"s1"}', 'hello')
		returning id::text
	`)
	insertScopeRow(t, tx, "a direct message", `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'hello', 'session:s1')
		returning id::text
	`)

	documents := []seedDocument{
		{"the issue's spec", spec},
		{"a document of the issue that is not its spec", notes},
		{"a document with no sources", quiet},
		{"a project document", handbook},
		{"the other issue's spec", otherSpec},
		{"an id no document carries", "6f1c1f39-3d43-4b8e-9d0a-2f0d6c5b7a10"},
	}

	for _, document := range documents {
		arms := scopeSourcesBy(t, tx, scopeSourcesQuery, document.id)
		one := scopeSourcesBy(t, tx, scopeSourcesOneSeedQuery, document.id)
		if !slices.Equal(arms, one) {
			t.Errorf("the sources of %s are %v, where the one `or`-ed seed answers %v", document.name, arms, one)
		}
		// Each comment the arms answer is one ScopeOf puts in this document: the walk and its
		// inverse answer the same question.
		for _, source := range arms {
			if source.Kind != "comment" {
				continue
			}
			scope, err := ScopeOf(ctx, tx, source.Kind, source.ID)
			if err != nil {
				t.Fatalf("the scope of comment %s: %v", source.ID, err)
			}
			if scope != document.id {
				t.Errorf("the sources of %s hold comment %s, whose scope is %q", document.name, source.ID, scope)
			}
		}
	}

	for n, arm := range scopeSourcesSeedArms {
		without := scopeSourcesWithoutArm(t, n)
		changed := false
		for _, document := range documents {
			if !slices.Equal(scopeSourcesBy(t, tx, without, document.id), scopeSourcesBy(t, tx, scopeSourcesQuery, document.id)) {
				changed = true
			}
		}
		if !changed {
			t.Errorf("dropping the arm over %s (%s) left every document's sources unchanged, so the fixture reaches that arm through nothing and the comparison above proves nothing about it",
				arm.name, arm.index)
		}
	}
}

// scopeSeedScaleIssues and scopeSeedScaleComments are the size the plan below is read at: a few
// thousand comments over forty issues, enough that reading one document's comments through an
// index beats a scan of the table and small enough to build in a test. The deployment INFRA-988
// measured holds 494,000 comments, where the scan cost 867.7 ms a document.
const (
	scopeSeedScaleIssues   = 40
	scopeSeedScaleComments = 4000
)

// The seed runs once per document the block index re-indexes (docs/block_index.go:858), so it is
// held to reading comments through an index: no scan of the whole table, and every arm read
// through the index it is written for, keyed on that index's own first column.
//
// The control is the one `or`-ed seed this replaced, planned over the same rows: it scans
// comments, which is the plan this change is here to stop. A test whose control did not scan
// would be proving nothing.
func TestScopeSourcesSeedReadsCommentsThroughItsIndexes(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	spec := buildScopeSeedScale(t, tx)

	plan, text := scopeSeedPlan(t, tx, scopeSourcesQuery, spec)
	t.Logf("the arms, at %d comments:\n%s", scopeSeedScaleComments, text)
	if explaintest.SeqScansRelation(t, plan, "comments") {
		t.Errorf("the sources of a document scan comments whole at %d comments:\n%s", scopeSeedScaleComments, text)
	}
	keys := explaintest.NewIndexKeys(database.Pool)
	for _, arm := range scopeSourcesSeedArms {
		keys.RequireIndexKeyed(t, plan, arm.index, "the seed's arm over "+arm.name, explaintest.CondRequired)
	}

	control, controlText := scopeSeedPlan(t, tx, scopeSourcesOneSeedQuery, spec)
	t.Logf("the one `or`-ed seed, at %d comments:\n%s", scopeSeedScaleComments, controlText)
	if !explaintest.SeqScansRelation(t, control, "comments") {
		t.Errorf("the one `or`-ed seed did not scan comments at %d of them, so this test's control proves nothing:\n%s",
			scopeSeedScaleComments, controlText)
	}
}

// buildScopeSeedScale fills tx with forty issues of a hundred comments each and answers the spec
// of the first, the document the plans are read for. Every arm reaches that document: the
// comments of its own issue, the comments anchored to it, the comments it owns and the replies
// to an ask of its issue. Its issue's comments are a fortieth of the table, which is the shape
// the deployment has, where one issue's thread is a small part of every comment there is.
func buildScopeSeedScale(t *testing.T, tx pgx.Tx) string {
	t.Helper()
	ctx := context.Background()
	createReferenceProject(t, tx, "ACME")
	for number := 1; number <= scopeSeedScaleIssues; number++ {
		insertSeedIssue(t, tx, fmt.Sprintf("ACME-%d", number), "ACME", number)
	}
	if _, err := tx.Exec(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		select 'ACME-' || n, 'ACME', 'spec', 'spec', 'doc', true, '{"kind":"user","id":"alice"}'
		from generate_series(1, $1) n
	`, scopeSeedScaleIssues); err != nil {
		t.Fatalf("create the specs: %v", err)
	}
	var spec string
	if err := tx.QueryRow(ctx, `select id::text from artifacts where issue_key = 'ACME-1' and is_primary`).Scan(&spec); err != nil {
		t.Fatalf("read the spec of ACME-1: %v", err)
	}
	ask := insertScopeAsk(t, tx, "ACME-1", "", nil)
	if _, err := tx.Exec(ctx, `
		insert into comments (issue_key, author, body)
		select 'ACME-' || (n % $1 + 1), '{"kind":"user","id":"alice"}', 'comment ' || n
		from generate_series(1, $2) n
	`, scopeSeedScaleIssues, scopeSeedScaleComments); err != nil {
		t.Fatalf("create the comments: %v", err)
	}
	// The anchored comments are in another issue, so the arm that answers them is the anchor's
	// own and not the issue's.
	if _, err := tx.Exec(ctx, `
		insert into comments (issue_key, author, body, anchor)
		select 'ACME-2', '{"kind":"user","id":"alice"}', 'anchored ' || n,
		       jsonb_build_object('artifact_id', $1::text, 'mark_id', 'm' || n)
		from generate_series(1, 5) n
	`, spec); err != nil {
		t.Fatalf("create the anchored comments: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into comments (artifact_id, author, body)
		select $1, '{"kind":"user","id":"alice"}', 'owned ' || n from generate_series(1, 5) n
	`, spec); err != nil {
		t.Fatalf("create the owned comments: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into comments (issue_key, author, body, ask_id)
		select 'ACME-1', '{"kind":"user","id":"alice"}', 'ask reply ' || n, $1 from generate_series(1, 5) n
	`, ask); err != nil {
		t.Fatalf("create the ask replies: %v", err)
	}
	// A thread the walk descends, so the plan carries the recursive term's read too.
	parent := insertScopeRow(t, tx, "a thread root of ACME-1", `
		insert into comments (issue_key, author, body) values ('ACME-1', '{"kind":"user","id":"alice"}', 'root')
		returning id::text
	`)
	for depth := 1; depth <= 3; depth++ {
		parent = insertScopeRow(t, tx, "a reply in that thread", `
			insert into comments (issue_key, author, body, reply_to)
			values ('ACME-1', '{"kind":"user","id":"alice"}', 'reply', $1) returning id::text
		`, parent)
	}
	if _, err := tx.Exec(ctx, `
		insert into messages (issue_key, author, body)
		select 'ACME-' || (n % $1 + 1), '{"kind":"session","id":"s1"}', 'message ' || n
		from generate_series(1, $1) n
	`, scopeSeedScaleIssues); err != nil {
		t.Fatalf("create the messages: %v", err)
	}
	// The planner reads row counts and column statistics, and a fixture nothing analyzed is
	// planned as if it were almost empty, where a scan of the table is the right plan for any
	// read of it.
	if _, err := tx.Exec(ctx, `analyze comments, artifacts, asks, messages, issues`); err != nil {
		t.Fatalf("analyze the fixture: %v", err)
	}
	return spec
}

// scopeSeedPlan is the plan Postgres ran for statement over the document doc, as the checks read
// it and as a person reads it. Sequential scans are left on, unlike the plan tests that pin one
// index over a small fixture: the question here is which plan the planner really chooses at this
// size, and a setting that forbade the scan would answer it for him.
func scopeSeedPlan(t *testing.T, tx pgx.Tx, statement, doc string) (json.RawMessage, string) {
	t.Helper()
	ctx := context.Background()
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+statement, doc).Scan(&raw); err != nil {
		t.Fatalf("explain the sources of %s: %v", doc, err)
	}
	rows, err := tx.Query(ctx, "explain (analyze, buffers) "+statement, doc)
	if err != nil {
		t.Fatalf("explain the sources of %s as text: %v", doc, err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the plan of the sources of %s: %v", doc, err)
	}
	return explaintest.OnePlan(t, raw), strings.Join(lines, "\n")
}
