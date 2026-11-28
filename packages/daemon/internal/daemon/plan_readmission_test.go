// The plan an owner gives for a child of its tree before the daemon records the child, through the
// re-admission of the tree's root. The test boots the whole daemon over the real Dispatch of this
// package's tests and walks the root from its owner's plan (ownerStepsRig, owner_steps_rig_test.go);
// each fact reaches the daemon through its own routes, its own reads and the stream it consumes.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The architect of a tree gives the plan `implementer reviewer` for a child the daemon has not
// recorded, which stores it at generation 0. The root walks its own plan to its merge, where it
// waits in verifying, since the child in backlog holds its close (Dispatch lists it open). A person
// closes the root, the linger closes its tree, and a person sets the root todo again, which
// re-admits it at generation 2. The
// re-admission clears the facts of the tree's recorded issues (record.ClearTreeGeneration) and no
// plan row: the child's rows at generation 0 stand as given, and the root's plan stays at
// generation 1. The architect of a tree is the claim of its root's key at every generation
// (record.ArchitectOfTree), so the re-admitted root's architect still owns both plans. The root's
// gate opening again starts the root at implementing on its own plan, carried to generation 2 from
// its start, and the architect's release of the child starts the child at implementing, the first
// step of the plan given for it, carried to the child's generation from its start; no planner of
// the child runs. The control: the same release of a child its owner gave no plan starts planning,
// the first of the project's default steps, and runs no implementer.
func TestAPlanGivenBeforeAChildIsReleasedWaitsThroughItsRootsReadmission(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		given        bool
		first        phase.Phase
		starts, none claim.Role
	}{
		{"the plan given at generation 0 starts the released child at implementing", true, phase.Implementing, claim.RoleImplementer, claim.RolePlanner},
		{"control: a child its owner gave no plan starts planning", false, phase.Planning, claim.RolePlanner, claim.RoleImplementer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 1, config.Project{})
			root, architect := r.key, r.token(claim.RoleArchitect)
			child := r.childInBacklog()
			var given *record.StoredPlan
			if tc.given {
				artifact := r.documentOf(child, "plan.md", planOf("implementer", "reviewer"))
				if status, body := r.registerPlanOf(child, artifact, 1); status != http.StatusOK {
					t.Fatalf("register_plan of %s's plan = %d %s, want 200", child, status, body)
				}
				given = r.storedPlan(child, 0)
				if given == nil || given.Version.Source != record.PlanSourceOwner || given.Version.ArtifactID != artifact || given.Version.Version != 1 ||
					given.Version.GivenBy != architect {
					t.Fatalf("the plan of %s at generation 0 = %+v, want version 1 of %s given by %s", child, given, artifact, architect)
				}
			}
			r.givePlan("implementer", "reviewer")
			r.openGate(phase.Implementing)
			r.walkSteps([]string{"implementer", "reviewer"})
			r.waitPhaseOf(root, phase.Verifying)
			r.doneByPerson()
			r.waitLingerEnded(1)
			ran := r.storedPlan(root, 1)
			if ran == nil || len(ran.Tasks) != 1 || ran.Tasks[0].State != record.TaskDone {
				t.Fatalf("the plan of %s at generation 1 after its merge = %+v, want its task a done", root, ran)
			}

			launched := len(r.launchesOfRole(claim.RoleArchitect))
			r.todoAgain()
			r.waitStands(root, 2, phase.Admitted)
			if got := r.storedPlan(child, 0); !reflect.DeepEqual(got, given) {
				t.Fatalf("the plan of %s at generation 0 after the re-admission of %s = %+v, want %+v as given", child, root, got, given)
			}
			if got := r.storedPlan(root, 1); !reflect.DeepEqual(got, ran) {
				t.Fatalf("the plan of %s at generation 1 after its re-admission = %+v, want %+v as it ran", root, got, ran)
			}
			if got := r.storedPlan(root, 2); got != nil {
				t.Fatalf("%s holds the plan %+v at generation 2 before its gate opens, want none: its first step writes it", root, got)
			}
			if _, recorded := r.d.state().Issues[child]; recorded {
				t.Fatalf("%s is recorded before its release, want it unrecorded", child)
			}

			// The linger's close of the tree suspended the architect, and the re-admission resumes it
			// in its own session, whose pane comes up in that launch (reattach).
			testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the resume of "+string(architect), func() bool {
				return len(r.launchesOfRole(claim.RoleArchitect)) > launched
			})
			r.reattach(r.worker(claim.RoleArchitect))
			r.openGate(phase.Implementing)
			again := r.storedPlan(root, 2)
			if again == nil || again.Version.Source != record.PlanSourceOwner || again.Version.ArtifactID != ran.Version.ArtifactID ||
				len(again.Tasks) != 1 || again.Tasks[0].State != record.TaskRunning || again.Tasks[0].Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) ||
				!reflect.DeepEqual(again.Tasks[0].Counts, record.ReturnCounts{}) {
				t.Fatalf("the plan of %s at generation 2 = %+v, want its owner's plan of generation 1 from its start, task a running at step 1", root, again)
			}

			status, body := r.d.request(http.MethodPost, "/legion/v1/waves/release",
				api.WaveReleaseRequest{GrantID: r.worker(claim.RoleArchitect).grant(), Issues: []string{child}}, false)
			if status != http.StatusOK {
				t.Fatalf("release_children of %s = %d %s, want 200", child, status, body)
			}
			r.waitPhaseOf(child, tc.first)
			r.awaitLaunch(r.tokenOf(child, tc.starts))
			if launches := r.issueLaunches(child); slices.Contains(launches, "Spawn "+string(tc.none)) || slices.Contains(launches, "Resume "+string(tc.none)) {
				t.Fatalf("the release of %s launched %v, want no %s", child, launches, tc.none)
			}
			generation := r.d.state().Issues[child].Generation
			taken := r.storedPlan(child, generation)
			if taken == nil || len(taken.Tasks) != 1 || taken.Tasks[0].State != record.TaskRunning ||
				taken.Tasks[0].Place != (record.TaskPlace{Kind: record.PlaceStep, Position: 1}) {
				t.Fatalf("the plan of %s at generation %d = %+v, want one task running at step 1", child, generation, taken)
			}
			if !tc.given {
				if taken.Version.Source != record.PlanSourceDefault {
					t.Fatalf("the plan of %s at generation %d is %+v, want the project's default steps", child, generation, taken.Version)
				}
				return
			}
			if taken.Version.Source != record.PlanSourceOwner || taken.Version.ArtifactID != given.Version.ArtifactID || taken.Version.Version != 1 ||
				taken.Version.GivenBy != architect || taken.Tasks[0].Task != "a" || stepRoles(taken.Steps["a"]) != "implementer reviewer" {
				t.Fatalf("the plan of %s at generation %d = %+v, want version 1 of %s given by %s, task a with the steps implementer reviewer",
					child, generation, taken, given.Version.ArtifactID, architect)
			}
			if got := r.storedPlan(child, 0); !reflect.DeepEqual(got, given) {
				t.Fatalf("the plan of %s at generation 0 after its release = %+v, want %+v as given", child, got, given)
			}
		})
	}
}

// childInBacklog creates a child of the walk's issue in Dispatch, as a person, and moves it to
// backlog, where an architect's release_children takes it from. The daemon records nothing of it
// until that release. It answers the child's key.
func (r *ownerStepsRig) childInBacklog() string {
	r.t.Helper()
	var created createdIssue
	r.person(http.MethodPost, "/api/v1/issues", map[string]any{"project": r.cfg.Project, "title": "A child released after its root runs again",
		"parent": r.key}, http.StatusCreated, &created)
	r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": "backlog"}, http.StatusOK, nil)
	return created.Key
}

// storedPlan is the plan of issue at generation as the daemon's store holds it, nil for none.
func (r *ownerStepsRig) storedPlan(issue string, generation uint64) *record.StoredPlan {
	r.t.Helper()
	ctx := context.Background()
	var stored *record.StoredPlan
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stored, err = r.records.Plan(ctx, tx, issue, generation)
		return err
	}); err != nil {
		r.t.Fatalf("read the plan of %s at generation %d: %v", issue, generation, err)
	}
	return stored
}

// stepRoles is the roles of steps in their order, one space between two.
func stepRoles(steps []record.PlanStep) string {
	roles := make([]string, len(steps))
	for i, step := range steps {
		roles[i] = step.Role
	}
	return strings.Join(roles, " ")
}

// waitLingerEnded waits for the record to hold the end of the linger of the walk's issue at
// generation as applied (intake.LingerExpired): the outbox applies it once the linger's time has
// passed, and it closes the tree.
func (r *ownerStepsRig) waitLingerEnded(generation uint64) {
	r.t.Helper()
	event := fmt.Sprintf("linger:%s:%d", r.key, generation)
	testwait.WithinPolling(r.t, stepRecordsWait, 100*time.Millisecond, "the end of the linger of "+r.key, func() bool {
		var applied int
		if err := r.st.Pool().QueryRow(context.Background(), `select count(*) from processed_events where source = 'outbox' and event_id = $1`,
			event).Scan(&applied); err != nil {
			r.t.Fatalf("count the end of the linger of %s: %v", r.key, err)
		}
		return applied == 1
	})
}

// todoAgain is a person setting the walk's issue todo in Dispatch, and the event Dispatch relays for
// that write, which the daemon's consumer reads as it reads any issue event.
func (r *ownerStepsRig) todoAgain() {
	t := r.t
	t.Helper()
	var moved createdIssue
	r.person(http.MethodPatch, "/api/v1/issues/"+r.key, map[string]any{"status": "todo"}, http.StatusOK, &moved)
	payload, err := json.Marshal(map[string]any{
		"id": moved.LastSeq, "issue_key": r.key, "project": r.cfg.Project, "seq": moved.LastSeq, "type": "issue.updated",
		"actor": map[string]string{"kind": "user", "id": "a-person"}, "notify": true, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"key": r.key, "status": "todo", "title": r.title(), "parent": nil, "rank": moved.Rank,
			"labels": liveDispatchLabels(true), "last_seq": moved.LastSeq, "project": r.cfg.Project, "resolved_repo": liveRepo,
		},
	})
	if err != nil {
		t.Fatalf("encode the Dispatch event of %s: %v", r.key, err)
	}
	r.publish("notifications.dispatch.issue."+r.key+".issue.updated", "dispatch", fmt.Sprintf("%s-todo-%d", r.key, moved.LastSeq), string(payload))
}

// waitStands waits for issue to stand in want at generation.
func (r *ownerStepsRig) waitStands(issue string, generation uint64, want phase.Phase) {
	r.t.Helper()
	var last api.Issue
	if !testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool {
		last = r.d.state().Issues[issue]
		return last.Generation == generation && last.Phase == want
	}) {
		r.t.Fatalf("%s stands in %q at generation %d, want %s at %d", issue, last.Phase, last.Generation, want, generation)
	}
}

// doneByPerson is a person setting the walk's issue done in Dispatch, and the event Dispatch relays
// for that write, which the daemon's consumer reads as it reads any issue event.
func (r *ownerStepsRig) doneByPerson() {
	t := r.t
	t.Helper()
	var moved createdIssue
	r.person(http.MethodPatch, "/api/v1/issues/"+r.key, map[string]any{"status": "done"}, http.StatusOK, &moved)
	payload, err := json.Marshal(map[string]any{
		"id": moved.LastSeq, "issue_key": r.key, "project": r.cfg.Project, "seq": moved.LastSeq, "type": "issue.closed",
		"actor": map[string]string{"kind": "user", "id": "a-person"}, "notify": true, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"key": r.key, "status": "done", "title": r.title(), "parent": nil, "rank": moved.Rank,
			"labels": liveDispatchLabels(true), "last_seq": moved.LastSeq, "project": r.cfg.Project, "resolved_repo": liveRepo,
		},
	})
	if err != nil {
		t.Fatalf("encode the Dispatch event of %s: %v", r.key, err)
	}
	r.publish("notifications.dispatch.issue."+r.key+".issue.closed", "dispatch", fmt.Sprintf("%s-done-%d", r.key, moved.LastSeq), string(payload))
}
