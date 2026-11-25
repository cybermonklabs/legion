package record_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// The served state names each start that waits for its issue's pod by the claim it starts, as
// the state names an agent of a role everywhere else: by the role and the claim's token. Two
// claims of one issue can hold one role (record.WorkerClaim), so the role alone names a second
// agent of the role as the role's own agent. Here the start of `implementer-2`, a second agent of
// the implementer role whose slot the pod lacks, waits beside a start of the role's own agent
// that waits too, and the two are served apart. A start of the release before names no claim,
// and it is named by the claim the record gives its role's step, the claim the outbox's next try
// of the row acts on: the tester's own here, since the issue has no phases rows. It reads
// record.Postgres.StartsWaitingForPod's rows through projection.Project, so it is a test of this
// package's read, in this package's external test package, since projection imports record.
func TestTheStateNamesEachStartThatWaitsForItsPodByTheClaimItStarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := testpg.ClonePool(t, "legion_record_test")
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "root",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U",
		LastDispatchSeq: 1}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutIssue(ctx, tx, issue)
	}); err != nil {
		t.Fatalf("put %s: %v", issue.Key, err)
	}
	roleToken := func(role claim.Role) claim.Token {
		t.Helper()
		got, err := claim.NewToken("legion", issue.Key, role)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	own, tester := roleToken(claim.RoleImplementer), roleToken(claim.RoleTester)
	second, err := claim.AgentToken("legion", issue.Key, "implementer-2")
	if err != nil {
		t.Fatal(err)
	}
	// Each start as a try of the outbox leaves it when the issue's pod lacks the start's slot or
	// role (the outbox's roleWaitsForItsPod): its last error begins with
	// pods.StartWaitsForPodPrefix, and it is due again in an hour.
	waits := func(role claim.Role, token claim.Token, slot string) {
		t.Helper()
		payload := map[string]any{"op": "start", "tree": issue.Tree, "role": role, "generation": 1}
		if token != "" {
			payload["claim"] = token
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		text := pods.StartWaitsForPodPrefix + string(role) + " of " + issue.Key +
			": the pod pod-1 was built without " + slot
		if _, err := pool.Exec(ctx, `insert into outbox
			(kind, issue, payload, attempts, next_at, last_error)
			values ('supervise', $1, $2::jsonb, 1, now() + interval '1 hour', $3)`,
			issue.Key, string(encoded), text); err != nil {
			t.Fatalf("queue the waiting start of %s: %v", slot, err)
		}
	}
	waits(claim.RoleImplementer, own, "implementer")
	waits(claim.RoleImplementer, second, "implementer-2")
	waits(claim.RoleTester, "", "tester")

	var served map[string]any
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		state, err := projection.Project(ctx, projection.Inputs{Tx: tx, Store: records,
			Project: issue.Project, Now: time.Now().UTC()})
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(state.Issues[issue.Key])
		if err != nil {
			return err
		}
		return json.Unmarshal(encoded, &served)
	}); err != nil {
		t.Fatalf("project the state: %v", err)
	}
	want := []any{
		map[string]any{"role": string(claim.RoleImplementer), "token": string(own)},
		map[string]any{"role": string(claim.RoleImplementer), "token": string(second)},
		map[string]any{"role": string(claim.RoleTester), "token": string(tester)},
	}
	if got := served["startWaitsForPod"]; !reflect.DeepEqual(got, want) {
		t.Errorf("%s is served startWaitsForPod %v, want %v", issue.Key, got, want)
	}
}
