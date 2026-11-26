package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// backfillDecision is the decision block of the spec the backfill tests settle, which alice answers.
const backfillDecision = ":::ask{#region urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"

// A backfill command settles a document that a server left owing a settlement, and that
// settlement carries a person's approval as the server's own would: through the form gate of The
// Spec that DISPATCH_SPEC_ROOTS names. ACME-2's spec is under the root ACME-1. alice approved its
// version 1, then answered its decision block, and the server stopped before it settled her
// answer. backfill-block-ids stores her answer as version 2: where the spec's statement keeps the
// form, her approval is carried to version 2; where it breaks the form, nothing is carried, as
// her own Approve of version 2 would be refused. Each row also reads that the backfill wrote the
// version, so a run that settled nothing carries nothing and fails the first row.
func TestABackfillCarriesAnApprovalThroughTheFormGate(t *testing.T) {
	for _, row := range []struct {
		name      string
		statement string
		carried   bool
	}{
		{name: "a text that keeps the form", statement: "Dispatch stores each comment.", carried: true},
		{name: "a text that breaks the form", statement: "Dispatch stores each comment. Dispatch indexes each comment. Dispatch shows each comment."},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			database := storetest.Open(t)
			artifactID := leaveAnswerUnsettled(t, database, "# Spec\n\n:::requirement{#store-comment}\n"+row.statement+"\n:::\n\n"+backfillDecision)

			var stdout, stderr bytes.Buffer
			env := envGetter(map[string]string{
				"DATABASE_URL":        database.Pool.Config().ConnString(),
				"DISPATCH_SPEC_ROOTS": "ACME-1",
				"XDG_CONFIG_HOME":     t.TempDir(),
			})
			if code := runSubcommand(context.Background(), []string{"backfill-block-ids"}, env, &stdout, &stderr); code != 0 {
				t.Fatalf("backfill-block-ids: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
			}
			if versions := backfillVersions(t, database, artifactID); versions != 2 {
				t.Fatalf("the spec has %d versions after the backfill, want 2: the backfill stores alice's answer", versions)
			}
			reviews, approvals := backfillReviews(t, database, artifactID)
			if row.carried && (reviews != 2 || approvals != 1) {
				t.Fatalf("reviews = %d, artifact.approved events = %d; want alice's approval carried to version 2", reviews, approvals)
			}
			if !row.carried && (reviews != 1 || approvals != 0) {
				t.Fatalf("reviews = %d, artifact.approved events = %d; want alice's approval of version 1 alone and no event", reviews, approvals)
			}
		})
	}
}

// leaveAnswerUnsettled makes markdown the spec of ACME-2, a child of ACME-1, at version 1, which
// alice approves. A document service with a settle window of an hour, a server's, indexes the
// decision block region and then writes alice's answer to it, and stops before the window ends
// (Quiesce: its rooms are written to the store and the settlement is called off). The document
// then owes a settlement that no version holds yet. It returns the spec's id.
func leaveAnswerUnsettled(t *testing.T, database *store.Store, written string) string {
	t.Helper()
	ctx := context.Background()
	// Version 1 holds the markdown as the renderer writes it, so the room seeded from it renders
	// the same text and its settlement writes no version of its own.
	tree, err := pmdoc.Parse(written)
	if err != nil {
		t.Fatalf("parse the spec: %v", err)
	}
	markdown, err := pmdoc.Render(pmdoc.StripAnchorMarks(tree))
	if err != nil {
		t.Fatalf("render the spec: %v", err)
	}
	var artifactID string
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('ACME', 'Acme', 'alice')`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('ACME-1', 'ACME', 1, 'The root of The Spec', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into issues (key, project_key, number, title, created_by, rank, parent_key) values ('ACME-2', 'ACME', 2, 'The queue', '{"kind":"user","id":"alice"}', 'V', 'ACME-1')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the issues: %v", err)
		}
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('ACME-2', 'ACME', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&artifactID); err != nil {
		t.Fatalf("create the spec: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
	`, artifactID, markdown); err != nil {
		t.Fatalf("create version 1: %v", err)
	}

	server := docs.New(docs.Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	backfillJoinedWrite(t, database, server, func(ctx context.Context, _ pgx.Tx) error {
		_, err := server.SeedText(ctx, artifactID, markdown, model.Actor{Kind: "user", ID: "seed"})
		return err
	})
	server.SettleNow(artifactID)
	testwait.Eventually(t, "the settlement to index the decision block", func() bool {
		var rows int
		if err := database.Pool.QueryRow(ctx, `select count(*) from asks where block_artifact_id = $1 and block_id = 'region'`, artifactID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows == 1
	})
	if versions := backfillVersions(t, database, artifactID); versions != 1 {
		t.Fatalf("the seed's settlement left %d versions, want version 1 alone", versions)
	}
	if _, err := database.Pool.Exec(ctx, `insert into artifact_reviews (artifact_id, version, state, actor) values ($1, 1, 'approved', '{"kind":"user","id":"alice"}')`, artifactID); err != nil {
		t.Fatalf("approve version 1 as alice: %v", err)
	}

	at := time.Now().UTC()
	answer, err := json.Marshal(model.AskAnswer{User: "alice", Selected: []string{"East"}, At: at})
	if err != nil {
		t.Fatal(err)
	}
	backfillJoinedWrite(t, database, server, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `update asks set state = 'answered', answer = $2 where block_artifact_id = $1 and block_id = 'region'`, artifactID, answer); err != nil {
			return err
		}
		return server.SetBlockAttributes(ctx, artifactID, "region", map[string]any{
			"state": "answered", "answered_by": "alice", "answered_at": at.Format(time.RFC3339Nano), "selected": []string{"East"}, "answer": nil,
		}, model.Actor{Kind: "user", ID: "alice"})
	})
	if err := server.Quiesce(ctx); err != nil {
		t.Fatalf("stop the server's document service: %v", err)
	}
	var owed int
	if err := database.Pool.QueryRow(ctx, `select count(*) from doc_settlements_pending where artifact_id::text = $1`, artifactID).Scan(&owed); err != nil {
		t.Fatal(err)
	}
	if versions := backfillVersions(t, database, artifactID); owed != 1 || versions != 1 {
		t.Fatalf("the server left %d settlements owed and %d versions, want alice's answer owed one and no version 2", owed, versions)
	}
	return artifactID
}

// backfillJoinedWrite runs write in a transaction of database joined to service, as a route runs
// its writes, and commits it through the service's ledger.
func backfillJoinedWrite(t *testing.T, database *store.Store, service *docs.Service, write func(context.Context, pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if err := write(joined, tx); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// backfillVersions is how many versions the document has.
func backfillVersions(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var versions int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	return versions
}

// backfillReviews is how many reviews the document has, and how many artifact.approved events
// name it.
func backfillReviews(t *testing.T, database *store.Store, artifactID string) (reviews, approvals int) {
	t.Helper()
	ctx := context.Background()
	if err := database.Pool.QueryRow(ctx, `select count(*) from artifact_reviews where artifact_id = $1`, artifactID).Scan(&reviews); err != nil {
		t.Fatalf("count reviews: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `select count(*) from events where type = 'artifact.approved' and payload->>'artifact_id' = $1`, artifactID).Scan(&approvals); err != nil {
		t.Fatalf("count artifact.approved events: %v", err)
	}
	return reviews, approvals
}
