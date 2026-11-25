package workflow

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// gateRows is the number of design gates key records.
func gateRows(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(t.Context(), "select count(*) from design_gates where issue = $1", key).Scan(&rows); err != nil {
		t.Fatalf("read the gates of %s: %v", key, err)
	}
	return rows
}

// close_root and register_gate are the root's architect's requests on its root: each route judges
// the architect and reads the root before the fact lock (api's ownsOnChain), and the root's next
// run can open before the fact applies (NewRun, which clears the facts of the generation that
// ended, its design gates among them). The fact judges the generation its route read again
// (Engine.Apply): a close or a registration of the run that is over is refused, unspent, and
// closes or registers nothing of the run that stands. Each control is the same request with no new
// run between, and it acts: the close is queued for Dispatch (writeClose), and the gate is
// registered.
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
			return issueOf(t, pool, engine, key).PendingClose != nil
		}},
		{"register_gate", func(by claim.Token, read record.Issue) intake.Fact {
			return intake.GateRegistered{Issue: read.Key, ArtifactID: "spec-208", Version: 1, DispatchVersion: 1, By: by, Generation: read.Generation}
		}, func(t *testing.T, pool *pgxpool.Pool, _ *Engine, key string) bool {
			return gateRows(t, pool, key) == 1
		}},
	} {
		for _, rerun := range []bool{false, true} {
			name := request.name + ": control: the request of the run its route read"
			if rerun {
				name = request.name + ": the root's next run opened before the fact"
			}
			t.Run(name, func(t *testing.T) {
				pool := migratedPool(t)
				engine := returnEngine()
				read := stepRoot(t, pool, phase.Admitted)
				fact := request.fact(claim.Token(treeRoleToken(t, read.Key, claim.RoleArchitect)), read)
				if rerun {
					newRunOf(t, pool, engine, read.Key, "")
				}
				before, gates := standingOf(t, pool, engine, read.Key, false), gateRows(t, pool, read.Key)
				result := applyFacts(t, pool, engine)("request", fact)
				if !rerun {
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
