package store

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// agentResumeGenerationVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it. agentResumeGenerationMigration is
// its file.
const (
	agentResumeGenerationVersion   = 20261120042902
	agentResumeGenerationMigration = "20261120042902_claim_agent_resume_generation.up.sql"
)

// agentResumeGenerationRelease names the release whose way back the migration's header carries, as
// a failure to read that block names it.
const agentResumeGenerationRelease = "the release that marks a launch that had Oh My Pi resume a claim's session by its id"

// 20261120042902 adds claims.agent_resume_generation at its default, 0, and its release queues
// session-forgotten notices, a kind no earlier release decodes. Its way back, the last paragraph of
// the migration's header (migrations.WayBack), deletes the queued session-forgotten notices of both
// notice kinds. It drops the column and deletes the migration's version row. Every other column of
// claims, every claim and every other outbox row stays as it was. So the store is as it stood
// before the migration. The roll-forward's Migrate applies the file again and adds the column again
// at 0. The block is one transaction, opened by begin and closed by commit, and a block of another
// shape stops the test by name. The store checks alone passed with begin removed, so the test also
// reads the block's shape. The negative control is the store before the migration, whose claims
// hold no such column.
func TestTheAgentResumeGenerationsWayBackLeavesTheStoreAsBeforeTheMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, agentResumeGenerationVersion-1)
	const columns = `select column_name::text from information_schema.columns where table_name = 'claims' order by column_name`
	if before := rowsAsText(t, store, columns); slices.Contains(before, "agent_resume_generation") {
		t.Fatalf("the claims before the migration have the column agent_resume_generation, want none: %v", before)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the resume generation: %v", err)
	}
	migrated := rowsAsText(t, store, columns)
	if !slices.Contains(migrated, "agent_resume_generation") {
		t.Fatalf("the claims after the migration have no column agent_resume_generation: %v", migrated)
	}

	const token = "legion-LEGION-209-implementer"
	marked := tmuxClaim(token)
	marked.Generation, marked.SessionFile, marked.AgentResumeGeneration = 7, "", 7
	if err := store.PutClaim(ctx, marked); err != nil {
		t.Fatalf("put the marked claim: %v", err)
	}
	const mark = `select agent_resume_generation::text from claims order by token`
	if got := rowsAsText(t, store, mark); !reflect.DeepEqual(got, []string{"7"}) {
		t.Fatalf("the claim's mark after PutClaim = %v, want 7, the mark the way back then drops", got)
	}
	queue := func(kind, payload string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-209', $2::jsonb, 0, now(), '')`, kind, payload); err != nil {
			t.Fatalf("queue a %s row: %v", kind, err)
		}
	}
	queue("notice", `{"kind": "session-forgotten", "claim": "`+token+`"}`)
	queue("controller_notice", `{"kind": "session-forgotten", "claim": "`+token+`"}`)
	queue("notice", `{"kind": "session-lost", "claim": "`+token+`"}`)
	queue("controller_notice", `{"kind": "tick"}`)
	queue("dispatch_message", `{"kind": "session-forgotten", "body": "a message that names the kind"}`)

	block := wayBackOf(t, agentResumeGenerationVersion, agentResumeGenerationRelease)
	lines := strings.Split(block, "\n")
	if lines[0] != "begin;" {
		t.Fatalf("the way back of %s opens with %q, want %q: its statements must run as one transaction",
			agentResumeGenerationMigration, lines[0], "begin;")
	}
	if last := lines[len(lines)-1]; len(lines) < 2 || last != "commit;" {
		t.Fatalf("the way back of %s closes with %q, want %q: its statements must run as one transaction",
			agentResumeGenerationMigration, last, "commit;")
	}
	if _, err := store.Pool().Exec(ctx, block); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	want := slices.DeleteFunc(slices.Clone(migrated), func(column string) bool { return column == "agent_resume_generation" })
	if got := rowsAsText(t, store, columns); !reflect.DeepEqual(got, want) {
		t.Fatalf("the claims' columns after the way back = %v, want %v: the migration's column gone and every other kept", got, want)
	}
	const version = `select count(*)::text from schema_version where version = 20261120042902`
	if got := rowsAsText(t, store, version); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the migration's version rows after the way back = %v, want none", got)
	}
	const queued = `select kind || ' ' || payload::text from outbox order by id`
	if got, want := rowsAsText(t, store, queued), []string{
		`notice {"kind": "session-lost", "claim": "` + token + `"}`,
		`controller_notice {"kind": "tick"}`,
		`dispatch_message {"body": "a message that names the kind", "kind": "session-forgotten"}`,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the outbox after the way back = %q, want %q: each session-forgotten notice gone, every other row kept", got, want)
	}
	const claims = `select token from claims order by token`
	if got := rowsAsText(t, store, claims); !reflect.DeepEqual(got, []string{token}) {
		t.Fatalf("the claims after the way back = %v, want the claim the migration's column marked", got)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := rowsAsText(t, store, mark); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("the claim's mark after the roll-forward = %v, want the column again, at 0", got)
	}
}
