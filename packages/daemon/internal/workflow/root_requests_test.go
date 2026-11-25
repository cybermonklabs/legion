package workflow

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// readmitRoot leaves key done and writes its re-admission as admit's readmit writes it: the next
// generation at admitted with the status todo, its linger and hold cleared, and the facts of its
// tree's generation cleared (record.Store.ClearTreeGeneration), its design gates among them.
func readmitRoot(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	leaveAs(t, pool, engine, key, "done")
	onStored(t, pool, engine, key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		issue.Generation++
		issue.Phase, issue.Hold, issue.LingerUntil = phase.Admitted, nil, nil
		issue.Status, issue.DispatchStatus = "todo", "todo"
		if err := engine.store.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
		return engine.store.ClearTreeGeneration(ctx, tx, issue.Tree, stepClock)
	})
}

// gateRows is the number of design gates key records.
func gateRows(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(t.Context(), "select count(*) from design_gates where issue = $1", key).Scan(&rows); err != nil {
		t.Fatalf("read the gates of %s: %v", key, err)
	}
	return rows
}

// close_root and register_gate are the tree root's architect's requests on its root: each route
// judges the architect and reads the root before the fact lock (api's rootArchitectOf), and the
// root can leave and be admitted again at its next generation before the fact applies. The fact
// judges the generation its route read again (Engine.Apply): a close or a registration of the run
// that is over is refused, unspent, and closes or registers nothing of the run that stands. Each
// control is the same request with no re-admission between, and it acts.
func TestARootArchitectsRequestIsJudgedOnTheRecordItsFactReads(t *testing.T) {
	t.Parallel()
	for _, request := range []struct {
		name string
		fact func(by claim.Token, read record.Issue) intake.Fact
		// acted says whether the request did its work on the root that stands.
		acted func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) bool
	}{
		{"close_root", func(by claim.Token, read record.Issue) intake.Fact {
			return intake.CloseRoot{Issue: read.Key, Reason: "a person decided no change", By: by, Generation: read.Generation}
		}, func(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) bool {
			return issueOf(t, pool, engine, key).Phase == phase.Done
		}},
		{"register_gate", func(by claim.Token, read record.Issue) intake.Fact {
			return intake.GateRegistered{Issue: read.Key, ArtifactID: "spec-208", Version: 1, DispatchVersion: 1, By: by, Generation: read.Generation}
		}, func(t *testing.T, pool *pgxpool.Pool, _ *Engine, key string) bool {
			return gateRows(t, pool, key) == 1
		}},
	} {
		for _, readmitted := range []bool{false, true} {
			name := request.name + ": control: the request of the run its route read"
			if readmitted {
				name = request.name + ": the root left and was admitted again at its next generation before the fact"
			}
			t.Run(name, func(t *testing.T) {
				pool := migratedPool(t)
				engine := returnEngine()
				read := stepRoot(t, pool, phase.Admitted)
				fact := request.fact(claim.Token(treeRoleToken(t, read.Key, claim.RoleArchitect)), read)
				if readmitted {
					readmitRoot(t, pool, engine, read.Key)
				}
				before, gates := standingOf(t, pool, engine, read.Key, false), gateRows(t, pool, read.Key)
				result := applyFacts(t, pool, engine)("request", fact)
				if !readmitted {
					if result.Refusal != nil || !request.acted(t, pool, engine, read.Key) {
						t.Fatalf("the %s = %+v, want it to act", request.name, result.Refusal)
					}
					return
				}
				want := fact.(intake.ArchitectRequest).StaleGeneration(read.Generation + 1)
				if !reflect.DeepEqual(result.Refusal, want) || !result.Unspent {
					t.Fatalf("the %s = %+v (unspent %v), want the refusal %+v, unspent", request.name, result.Refusal, result.Unspent, want)
				}
				if now := standingOf(t, pool, engine, read.Key, false); !reflect.DeepEqual(now, before) || gateRows(t, pool, read.Key) != gates {
					t.Fatalf("the refused %s changed %s:\n before %+v (gates %d)\n after  %+v (gates %d)", request.name, read.Key, before, gates, now, gateRows(t, pool, read.Key))
				}
			})
		}
	}
}
