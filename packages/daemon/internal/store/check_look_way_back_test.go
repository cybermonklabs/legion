package store

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
// route needs the previous binary; this tree only holds the new explicit-field route. A done issue
// is done by its status as well as by its phase: a person's done keeps the phase the issue stood
// in (R20 of dispatch://INFRA-3320) and still opens a merged issue's production check, so
// LEGION-210, merged and set done by a person at merging, keeps its look as LEGION-209 does.
func TestTheWayBackClearsTheKeptLookOfAClaimOfARunningIssueAlone(t *testing.T) {
	s := migratedStore(t)
	for _, fixture := range []struct {
		key, project, status string
		phase                phase.Phase
		merged               bool
	}{
		{"LEGION-208", "LEGION", "in_progress", phase.Testing, false},
		{"LEGION-209", "LEGION", "in_progress", phase.Done, false},
		{"LEGION-210", "LEGION", "done", phase.Merging, true},
		{"OTHER-208", "OTHER", "in_progress", phase.Testing, false},
	} {
		if err := Transact(t.Context(), s.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			records := record.NewStore()
			if err := records.PutIssue(ctx, tx, record.Issue{Key: fixture.key, Project: fixture.project, Tree: fixture.key, Repo: "acme/widgets", GitHubRepo: "acme/widgets", DefaultBranch: "main", Title: "check", Phase: fixture.phase, Generation: 1, Status: fixture.status}); err != nil {
				return err
			}
			if !fixture.merged {
				return nil
			}
			return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestMerged, Issue: fixture.key, Repo: "acme/widgets",
				Number: 210, Branch: "legion/" + fixture.key, HeadSHA: "head", MergeSHA: "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0", Failing: []string{}})
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
	}{{"LEGION-208", 0}, {"LEGION-209", 2}, {"LEGION-210", 2}, {"OTHER-208", 2}} {
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
