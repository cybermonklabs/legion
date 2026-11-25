package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

func init() {
	registerEntryTestRows(entryTestRows{
		entry: projectReleaseContract,
		carry: withGoldenProjectRelease,
		// One body carries the whole release, every key path of `projectRelease` and every value
		// of its six enumerated fields, and no issue: the census serves each issue of a body again
		// through the record route at every contract, and the release is no issue's.
		states: func(*testing.T) []State {
			return []State{{ProjectRelease: releaseCarryingEveryDeclaredValue()}}
		},
		answers: func(*testing.T) map[AnswerID]answerFixture {
			return map[AnswerID]answerFixture{
				AnswerRefusal: {bodies: []any{projectReleaseRefusalFixture()}},
			}
		},
	})
	// The state only carries recorded releases. A dry run is never recorded.
	excludedDeclaredFields[answerField{answer: AnswerState, field: declaredField{typ: "api.ProjectReleaseResponse", name: "dryRun"}}] =
		"the state carries the project's newest recorded release, and a dry run is never recorded, so no state body carries `dryRun`"
}

func withGoldenProjectRelease(state State) State {
	release := goldenProjectRelease()
	state.ProjectRelease = &release
	return state
}

func projectReleaseRefusalFixture() PauseFailure {
	return releasePausedFailure(&store.ProjectPausedError{Project: "legion", Pause: controllerPause(false), Paused: true,
		Held: []store.HeldPausePod{{Issue: "LEGION-208", Pause: 7, HeldAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
			Claims: []claim.Token{"legion-legion-legion-208-implementer"}}}})
}

// The same daemon serves the full release from this entry on and omits it below the entry, and
// its log names `projectRelease` exactly when it omits it. Compare against the resolved number,
// which can be anywhere among the build's entries. Every other line is another band's, read from
// the table (narrowingsExpected), so a band a later entry adds is counted here and fails nothing.
func TestProjectReleaseStateContractBoundary(t *testing.T) {
	state := carriedState(t)
	at := entryNumber(projectReleaseContract)
	wantRelease, err := json.Marshal(state.ProjectRelease)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		stated int
	}{
		{"below cause times", causeTimesContract - 1},
		{"at the Dispatch credential", dispatchCredentialContract},
		{"just below the entry", at - 1},
		{"at the entry", at},
		{"current caller with no header or pod", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, lines := stateNarrowedStating(t, state, "192.0.2.1:41000", nil, tc.stated)
			// A caller that states no contract from no pod is served this daemon's own.
			contract := tc.stated
			if contract == 0 {
				contract = DaemonAPIVersion()
			}
			below := contract < at
			served, carried := keysOf(t, body)["projectRelease"]
			if below && carried {
				t.Errorf("caller at %d was served projectRelease %s", contract, served)
			}
			if !below && !bytes.Equal(served, wantRelease) {
				t.Errorf("caller at %d was served projectRelease %s, want %s", contract, served, wantRelease)
			}
			said := slices.ContainsFunc(lines, func(line narrowingLine) bool { return line.Dropped == "projectRelease" })
			if said != below {
				t.Errorf("caller at %d was written a line dropping projectRelease = %v, want %v: %+v", contract, said, below, lines)
			}
			wantNarrowings(t, lines, contract, narrowingsExpected(state, contract))
		})
	}
}

func TestProjectReleaseRefusalContractBoundary(t *testing.T) {
	refused := projectReleaseRefusalFixture()
	at := entryNumber(projectReleaseContract)
	older, _ := shapeAnswer(AnswerRefusal, refused, at-1, at-1)
	behind, ok := older.(PauseFailure)
	if !ok || len(behind.HeldPods) != 0 || behind.Failure != refused.Failure || behind.Pause != refused.Pause {
		t.Errorf("refusal below the entry = %+v, want the same pause refusal without heldPods", older)
	}
	current, _ := shapeAnswer(AnswerRefusal, refused, at, at)
	if !reflect.DeepEqual(current, refused) || len(refused.HeldPods) != 1 {
		t.Errorf("refusal at the entry = %+v, want the original refusal with its one held pod: %+v", current, refused)
	}
}

// releaseCarryingEveryDeclaredValue is a release that carries every key path of the state's
// `projectRelease` and every value of its six enumerated fields: the keys of the golden release
// (goldenProjectRelease), and those absent on that finished release, endedAt, endedBy and
// result.failure. The values are written here, and not read from the table, as
// stateCarryingEveryDeclaredValue's are.
func releaseCarryingEveryDeclaredValue() *ProjectReleaseResponse {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	roles := []claim.Role{claim.RoleArchitect, claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger}
	claimRoles := append(slices.Clone(roles), claim.RoleController)
	ends := []ReleasedClaimEnd{
		ReleasedClaimEnd(supervise.StateQueued), ReleasedClaimEnd(supervise.StateLaunchUncertain),
		ReleasedClaimEnd(supervise.StateLaunching), ReleasedClaimEnd(supervise.StateShimConnected),
		ReleasedClaimEnd(supervise.StateRegistered), ReleasedClaimEnd(supervise.StateReady),
		ReleasedClaimEnd(supervise.StateWorking), ReleasedClaimEnd(supervise.StateIdle),
		ReleasedClaimEnd(supervise.StateFailed), ReleasedClaimEnd(supervise.StateRetired),
		ReleasedClaimEnd(supervise.SuspendedByUnknown), ReleasedClaimEnd(supervise.SuspendedByOperator),
		ReleasedClaimEnd(supervise.SuspendedByDaemon), ReleasedClaimEnd(supervise.SuspendedByExit),
		ReleasedClaimEnd(supervise.SuspendedByReleased), ReleasedClaimEnd(supervise.SuspendedByProjectRelease),
		ReleasedClaimUnrecorded}
	asked := make([]ReleasedAsk, 0, len(roles))
	for _, role := range roles {
		asked = append(asked, ReleasedAsk{Role: role})
	}
	claims := make([]ReleasedClaim, 0, len(ends))
	for i, ended := range ends {
		role := claimRoles[i%len(claimRoles)]
		line := ReleasedClaim{Token: claim.Token(fmt.Sprintf("legion-values-values-%d-%s", i+1, role)),
			Role: role, Session: fmt.Sprintf("ses_values_%d", i+1), Ended: ended,
			BootStarts: ended == ReleasedClaimEnd(supervise.SuspendedByProjectRelease)}
		if role != claim.RoleController {
			line.Issue = fmt.Sprintf("VALUES-%d", i+1)
		}
		if ended == ReleasedClaimEnd(supervise.StateWorking) {
			line.Detail = "not stored suspended within the stop timeout of 20s after the later of its stop and the bound"
		}
		claims = append(claims, line)
	}
	answered, finished, ended := at.Add(10*time.Minute), at.Add(31*time.Minute), at.Add(2*time.Hour)
	lost, changed := 2, true
	return &ProjectReleaseResponse{
		ID: 9, StartedAt: at, BoundAt: at.Add(30 * time.Minute), Bound: (30 * time.Minute).String(),
		FinishedAt: &finished, EndedAt: &ended, EndedBy: store.ProjectReleaseEndedByBoot,
		Result: ProjectReleaseResult{
			Sandboxes: []ReleasedSandbox{
				{Name: "legion-values-values-1", Kind: runtime.ProjectSandboxIssue, Issue: "VALUES-1", Outcome: ReleaseWaiting, Asked: asked},
				{Name: "legion-values-values-2", Kind: runtime.ProjectSandboxIssue, Issue: "VALUES-2", Outcome: ReleaseDeletedClean},
				{Name: "legion-values-values-3", Kind: runtime.ProjectSandboxIssue, Issue: "VALUES-3", Outcome: ReleaseDeletedUnread},
				{Name: "legion-values-values-4", Kind: runtime.ProjectSandboxIssue, Issue: "VALUES-4", Outcome: ReleaseDeletedLost,
					Commits: &lost, WorkingCopyChanged: &changed,
					Asked: []ReleasedAsk{{Role: claim.RoleImplementer, AnsweredAt: &answered}}},
				{Name: "legion-values-values-5", Kind: runtime.ProjectSandboxIssue, Issue: "VALUES-5", Outcome: ReleaseDeletedReadRefused,
					Refusal: "Unknown command: read-unpushed", OtherImage: true,
					Asked: []ReleasedAsk{{Role: claim.RoleTester, Bound: true}}},
				{Name: "legion-values-controller", Kind: runtime.ProjectSandboxController, Outcome: ReleaseDeleted},
				{Name: "legion-values-cache-acme-widgets", Kind: runtime.ProjectSandboxCache, Outcome: ReleaseLeft, Reason: "its delete was refused"},
				{Name: "legion-values-probe", Kind: runtime.ProjectSandboxProbe, Outcome: ReleaseDeleted},
				{Name: "legion-values-unlabelled", Kind: runtime.ProjectSandboxUnknown, Outcome: ReleaseLeft, Reason: "no label names its kind"},
			},
			Claims:  claims,
			Failure: "list the project's Sandboxes again: the API server answered 503",
		},
	}
}

// goldenProjectRelease is a finished project release with one line of every kind a release
// writes: an issue pod deleted with nothing unpushed, one deleted with work lost after its asked
// role answered, one whose shim refused the read and whose asked role let the bound pass, one
// with no read, one left because an operator suspended a claim of it, the controller's pod, a
// repository cache whose delete the volume guard refused, and a claim line of each way a claim
// ends. It is the body of `GET /legion/v1/operator/project/release` and the state's
// `projectRelease`.
func goldenProjectRelease() ProjectReleaseResponse {
	started := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	answered, finished := started.Add(4*time.Minute), started.Add(31*time.Minute)
	one, changed, unchanged := 1, true, false
	none := 0
	return ProjectReleaseResponse{
		ID: 7, StartedAt: started, BoundAt: started.Add(30 * time.Minute), Bound: (30 * time.Minute).String(), FinishedAt: &finished,
		Result: ProjectReleaseResult{
			Sandboxes: []ReleasedSandbox{
				{Name: "legion-legion-cache-legion-legion", Kind: "cache", Outcome: ReleaseLeft,
					Reason: "sandbox runtime: refused to delete a Sandbox whose volume holds session files"},
				{Name: "legion-legion-controller", Kind: "controller", Outcome: ReleaseDeleted},
				{Name: "legion-legion-legion-208", Kind: "issue", Issue: "LEGION-208", Outcome: ReleaseDeletedClean,
					Commits: &none, WorkingCopyChanged: &unchanged},
				{Name: "legion-legion-legion-209", Kind: "issue", Issue: "LEGION-209", Outcome: ReleaseDeletedLost,
					Commits: &one, WorkingCopyChanged: &changed,
					Asked: []ReleasedAsk{{Role: claim.RoleImplementer, AnsweredAt: &answered}}},
				{Name: "legion-legion-legion-210", Kind: "issue", Issue: "LEGION-210", Outcome: ReleaseDeletedReadRefused,
					Refusal: "Unknown command: read-unpushed", OtherImage: true,
					Asked: []ReleasedAsk{{Role: claim.RoleTester, Bound: true}}},
				{Name: "legion-legion-legion-211", Kind: "issue", Issue: "LEGION-211", Outcome: ReleaseDeletedUnread},
				{Name: "legion-legion-legion-212", Kind: "issue", Issue: "LEGION-212", Outcome: ReleaseLeft,
					Reason: "an operator suspended its reviewer"},
			},
			Claims: []ReleasedClaim{
				{Token: "legion-legion-controller", Role: claim.RoleController, Session: "ses_controller", Ended: "project_release", BootStarts: true},
				{Token: "legion-legion-legion-208-architect", Issue: "LEGION-208", Role: claim.RoleArchitect, Session: "ses_architect",
					Ended: "project_release", BootStarts: true},
				{Token: "legion-legion-legion-209-implementer", Issue: "LEGION-209", Role: claim.RoleImplementer, Session: "ses_implementer",
					Ended: "project_release", BootStarts: true},
				{Token: "legion-legion-legion-210-tester", Issue: "LEGION-210", Role: claim.RoleTester, Session: "ses_tester",
					Ended: "daemon"},
				{Token: "legion-legion-legion-212-planner", Issue: "LEGION-212", Role: claim.RolePlanner, Session: "ses_planner",
					Ended: "working", Detail: "not stored suspended by operator within 20s: it stands working"},
			},
		},
	}
}

// The project release's own route answers the release as the state carries it.
func TestProjectReleaseResponseGolden(t *testing.T) {
	golden(t, "project-release.json", goldenProjectRelease())
}

// projectReleaseWords is project-release-words.json: every word of the project release's three
// finite fields, ReleasedSandbox.Outcome, ProjectReleaseResponse.EndedBy and ReleasedClaim.Ended.
type projectReleaseWords struct {
	Outcomes []ReleaseOutcome            `json:"outcomes"`
	EndedBy  []store.ProjectReleaseEnder `json:"endedBy"`
	Ended    []ReleasedClaimEnd          `json:"ended"`
}

// The project release's three finite fields are strict enums in
// `packages/contracts/src/legion-api.ts` (`LEGION_RELEASE_OUTCOMES`,
// `LEGION_PROJECT_RELEASE_ENDERS` and `LEGION_RELEASED_CLAIM_ENDS`), and a word the daemon writes
// that its enum does not name refuses the whole state body at every plugin that reads it. So the
// Go roster of each is written where the contracts suite reads it, as the failure causes are
// (TestFailureCausesGolden), and `legion-api.test.ts` holds each enum equal to its list, word for
// word and in order. The words are written out here beside it, so a constant renamed on the Go
// side fails here by its own name rather than rewriting the fixture.
func TestProjectReleaseWordsGolden(t *testing.T) {
	words := projectReleaseWords{
		Outcomes: ReleaseOutcomes,
		EndedBy:  []store.ProjectReleaseEnder{store.ProjectReleaseEndedByBoot},
		Ended:    ReleasedClaimEnds,
	}
	want := projectReleaseWords{
		Outcomes: []ReleaseOutcome{"waiting", "deleted_clean", "deleted_unread", "deleted_lost", "deleted_read_refused", "deleted", "left"},
		EndedBy:  []store.ProjectReleaseEnder{"boot"},
		Ended: []ReleasedClaimEnd{"queued", "launch_uncertain", "launching", "shim_connected", "registered", "ready", "working", "idle",
			"failed", "retired", "unknown", "operator", "daemon", "exit", "released", "project_release", "unrecorded"},
	}
	if !reflect.DeepEqual(words, want) {
		t.Fatalf("the project release's words = %q, want the wire words %q", words, want)
	}
	golden(t, "project-release-words.json", words)
}

// As with the numbered claim-role lists, a new role needs an arrival in each list that can
// carry it, or a strict reader will refuse the state.
func TestProjectReleaseRolesHaveRecordedArrivals(t *testing.T) {
	for _, role := range claim.Roles {
		for _, list := range []struct {
			name   string
			values []EnumeratedValue
		}{{"releaseAskRoles", releaseAskRoles}, {"releasedClaimRoles", releasedClaimRoles}} {
			if !slices.ContainsFunc(list.values, func(v EnumeratedValue) bool { return v.Value == string(role) }) {
				t.Errorf("%s does not record the arrival of claim role %q", list.name, role)
			}
		}
	}
}
