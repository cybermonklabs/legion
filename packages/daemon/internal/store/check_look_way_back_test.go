package store

//ci:reads docs/kubernetes.md the release's documented way back, read through ../../../../docs

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// This exercises the operator's SQL and its scope, not the previous daemon's route. That
// route needs the previous binary; this tree only holds the new explicit-field route.
func TestTheWayBackClearsTheKeptLookOfAClaimOfARunningIssueAlone(t *testing.T) {
	t.Parallel()
	s := migratedStore(t)
	for _, fixture := range []struct {
		key, project string
		phase        phase.Phase
	}{
		{"LEGION-208", "LEGION", phase.Testing},
		{"LEGION-209", "LEGION", phase.Done},
		{"OTHER-208", "OTHER", phase.Testing},
	} {
		if err := Transact(t.Context(), s.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			return record.NewStore().PutIssue(ctx, tx, record.Issue{Key: fixture.key, Project: fixture.project, Tree: fixture.key, Repo: "acme/widgets", GitHubRepo: "acme/widgets", DefaultBranch: "main", Title: "check", Phase: fixture.phase, Generation: 1, Status: "in_progress"})
		}); err != nil {
			t.Fatal(err)
		}
		c := tmuxClaim(claim.Token("claim-" + fixture.key))
		c.Issue, c.Tree, c.Project = fixture.key, fixture.key, fixture.project
		c.ServingCheckLook = supervise.CheckLook{Look: 2, OpenedAt: time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)}
		if err := s.PutClaim(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	guide, err := os.ReadFile("../../../../docs/kubernetes.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(guide), "#### Rolling back the release whose check report names its check\n")
	if !ok {
		t.Fatal("the release has no documented way back")
	}
	section, _, _ = strings.Cut(section, "\n#### ")
	_, sql, ok := strings.Cut(section, "```sql\n")
	if !ok {
		t.Fatal("the way back has no SQL block")
	}
	sql, _, ok = strings.Cut(sql, "```")
	if !ok {
		t.Fatal("the way back's SQL block does not end")
	}
	if _, err := s.Pool().Exec(t.Context(), strings.ReplaceAll(sql, "<PROJECT>", "LEGION")); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		key  string
		look int
	}{{"LEGION-208", 0}, {"LEGION-209", 2}, {"OTHER-208", 2}} {
		var look int
		var noOpened bool
		if err := s.Pool().QueryRow(t.Context(), "select serving_check_look, serving_check_opened_at is null from claims where issue = $1", fixture.key).Scan(&look, &noOpened); err != nil {
			t.Fatal(err)
		}
		if look != fixture.look || noOpened != (fixture.look == 0) {
			t.Fatalf("%s after the way back: look %d, absent opening %t; want look %d", fixture.key, look, noOpened, fixture.look)
		}
	}
}
