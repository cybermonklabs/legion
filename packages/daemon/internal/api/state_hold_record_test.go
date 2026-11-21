// This file reads the hold the way `read_record` reads it: from the issue's record in the body
// of GET /legion/v1/state, built over a real database as the daemon's own source builds it. It is
// an external test package for the reason waiting_reported_test.go is one: internal/projection
// builds that state and imports this package.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// holdPodUID is the pod the merger claims of this file run in, and holdPodAddr its address,
	// from which that pod's plugin asks for the state.
	holdPodUID  = "5c3a9e71-2b48-4d06-9f13-7a8e0c4d2b65"
	holdPodAddr = "10.1.24.9"
	// beforeTheHold is the contract before 207, the daemon contract that added each merger's
	// `hold` (version.go's 207): a plugin on it is served no hold key.
	beforeTheHold = 169
)

// holdSource is the daemon's own state source as it reads the hold (the daemon's `source`): the
// holds of the merger claims are judged by the daemon's one rule over the record, in the state
// route's own snapshot (workflow.HoldsNow, over workflow.HeldByDesignGate), and handed to the
// projection, which writes each on its claim's view.
type holdSource struct {
	records record.Store
	steps   []record.DesignStep
	claims  []supervise.Claim
}

func (s holdSource) State(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	holds, err := workflow.HoldsNow(ctx, tx, s.records, waitProject, s.steps, s.claims)
	if err != nil {
		return api.State{}, nil, err
	}
	state, err := projection.Project(ctx, projection.Inputs{
		Tx: tx, Store: s.records, Project: waitProject, Steps: s.steps, Claims: s.claims, Holds: holds, Now: waitNow,
	})
	return state, s.claims, err
}

// heldRecord is the issue record `read_record` hands the session for key, read from a state body
// as the legion tool reads it (recordFrom in packages/pi-shared/src/legion-phase-ops.ts): the
// body's issue of that key, whole.
func heldRecord(t *testing.T, body []byte, key string) map[string]json.RawMessage {
	t.Helper()
	var state struct {
		Issues map[string]map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state %s: %v", body, err)
	}
	issue, recorded := state.Issues[key]
	if !recorded {
		t.Fatalf("the daemon has no record for %s: %s", key, body)
	}
	return issue
}

// claimsHeld is the `hold` of every claim view of a record, raw, by role. A view carrying no key
// is in no entry.
func claimsHeld(t *testing.T, issue map[string]json.RawMessage) map[string]string {
	t.Helper()
	var workers map[string]struct {
		Claim map[string]json.RawMessage `json:"claim"`
	}
	if err := json.Unmarshal(issue["workers"], &workers); err != nil {
		t.Fatalf("decode the workers %s: %v", issue["workers"], err)
	}
	held := map[string]string{}
	for role, worker := range workers {
		if hold, carried := worker.Claim["hold"]; carried {
			held[role] = string(hold)
		}
	}
	if architect, carried := issue["architect"]; carried {
		var view map[string]json.RawMessage
		if err := json.Unmarshal(architect, &view); err != nil {
			t.Fatalf("decode the architect %s: %v", architect, err)
		}
		if hold, carried := view["hold"]; carried {
			held["architect"] = string(hold)
		}
	}
	return held
}

// `read_record` for the issue of a merger that the closed design gate holds carries the hold on
// that merger's claim: the gated step, the document registered for it and the version of it that
// is with its reviewer (dispatch://INFRA-2040 R4). The tree is built in a real database: the
// root's one gated step, the spec, holds version 3 with its reviewer while a person approved
// version 2, and its child stands in merging with its merger claimed. The rule is the outbox
// fence's (workflow.HeldByDesignGate), so no other claim carries a hold: the child's implementer,
// another role on the same issue; a merger of the same tree whose issue is in reviewing; and the
// merger of a tree whose spec stands approved at its latest version. A caller in a pod whose
// plugin records the contract before the hold's is served the same records with no hold key.
func TestReadRecordCarriesTheHoldOfAMergerTheClosedGateHolds(t *testing.T) {
	const (
		heldRoot  = "LEGION-230"
		heldChild = "LEGION-231"
		reviewing = "LEGION-232"
		openRoot  = "LEGION-240"
		openChild = "LEGION-241"
	)
	st, records, steps := waitingStore(t)
	ctx := context.Background()

	for _, seed := range []struct {
		key, tree string
		stands    phase.Phase
	}{
		{key: heldRoot, tree: heldRoot, stands: phase.Implementing},
		{key: heldChild, tree: heldRoot, stands: phase.Merging},
		{key: reviewing, tree: heldRoot, stands: phase.Reviewing},
		{key: openRoot, tree: openRoot, stands: phase.Implementing},
		{key: openChild, tree: openRoot, stands: phase.Merging},
	} {
		issue := record.Issue{Key: seed.key, Repo: waitRepo, DefaultBranch: "main", GitHubRepo: waitRepo,
			Project: waitProject, Title: seed.key, Tree: seed.tree, Phase: seed.stands, Generation: 1,
			Status: workflow.PhaseStatus(seed.stands), DispatchStatus: workflow.PhaseStatus(seed.stands),
			Rank: seed.key, HandedOver: true}
		if seed.key != seed.tree {
			parent := seed.tree
			issue.Parent = &parent
		}
		seedWaitIssue(t, st, records, issue, seed.key == seed.tree)
	}
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		// The held tree: a person approved version 2 of its spec, and version 3 is with the
		// reviewer. The open tree: a person approved its spec at the version Dispatch holds.
		heldApproval, openApproval := 2, 4
		if err := records.PutGate(ctx, tx, record.DesignGate{Issue: heldRoot, Step: record.DesignStepSpec,
			ArtifactID: "artifact-230", LatestVersion: 3, ApprovedVersion: &heldApproval, EverOpened: true}); err != nil {
			return err
		}
		return records.PutGate(ctx, tx, record.DesignGate{Issue: openRoot, Step: record.DesignStepSpec,
			ArtifactID: "artifact-240", LatestVersion: 4, ApprovedVersion: &openApproval, EverOpened: true})
	}); err != nil {
		t.Fatalf("seed the two gates: %v", err)
	}

	project, err := claim.ProjectToken(waitProject)
	if err != nil {
		t.Fatal(err)
	}
	// claimsAt is the four claims of the two trees, each in the pod at holdPodAddr and recording
	// contract, which is what the route cuts the state to for a caller from that pod.
	claimsAt := func(contract int) []supervise.Claim {
		var claims []supervise.Claim
		for _, held := range []struct {
			issue, tree string
			role        claim.Role
		}{
			{issue: heldChild, tree: heldRoot, role: claim.RoleMerger},
			{issue: heldChild, tree: heldRoot, role: claim.RoleImplementer},
			{issue: reviewing, tree: heldRoot, role: claim.RoleMerger},
			{issue: openChild, tree: openRoot, role: claim.RoleMerger},
		} {
			token, err := claim.NewToken(project, held.issue, held.role)
			if err != nil {
				t.Fatal(err)
			}
			claims = append(claims, supervise.Claim{Token: token, Project: project, Tree: held.tree, Issue: held.issue,
				Role: held.role, Generation: 1, State: supervise.StateIdle, Session: "ses_" + string(token),
				PluginContract: contract, Locator: &runtime.Locator{
					Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: holdPodUID + "/1",
					Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-" + held.issue, PodUID: holdPodUID,
						Container: "roles", Generation: 1},
				}})
		}
		return claims
	}
	stateFor := func(remote string, contract int) []byte {
		t.Helper()
		server := api.NewServer("127.0.0.1", 8437, api.Options{
			Steps: steps, State: holdSource{records: records, steps: steps, claims: claimsAt(contract)}, StateTransactions: st,
			CallerPod: func(addr netip.Addr) (string, bool) {
				return holdPodUID, addr == netip.MustParseAddr(holdPodAddr)
			},
		})
		request := httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil)
		request.RemoteAddr = remote
		recorder := httptest.NewRecorder()
		server.Handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("state = %d, want 200; body %s", recorder.Code, recorder.Body)
		}
		return recorder.Body.Bytes()
	}

	want := `{"step":"spec","artifactId":"artifact-230","version":3}`
	for name, ask := range map[string]struct {
		remote   string
		contract int
	}{
		"the merger's own pod, at this contract": {remote: holdPodAddr + ":41000", contract: api.DaemonAPIVersion},
		"the operator's own, from no pod":        {remote: "192.0.2.1:41000", contract: beforeTheHold},
	} {
		t.Run(name, func(t *testing.T) {
			body := stateFor(ask.remote, ask.contract)
			if got := claimsHeld(t, heldRecord(t, body, heldChild)); len(got) != 1 || got[string(claim.RoleMerger)] != want {
				t.Fatalf("read_record %s carries the holds %v, want the merger's alone, %s", heldChild, got, want)
			}
			for _, key := range []string{heldRoot, reviewing, openRoot, openChild} {
				if got := claimsHeld(t, heldRecord(t, body, key)); len(got) != 0 {
					t.Errorf("read_record %s carries the holds %v, want none: the closed gate holds a merger in merging alone", key, got)
				}
			}
		})
	}

	t.Run("a pod on the contract before the hold's", func(t *testing.T) {
		body := stateFor(holdPodAddr+":41000", beforeTheHold)
		for _, key := range []string{heldRoot, heldChild, reviewing, openRoot, openChild} {
			if got := claimsHeld(t, heldRecord(t, body, key)); len(got) != 0 {
				t.Errorf("read_record %s served to a pod at %d carries the holds %v, want no hold key: its plugin's schema does not name it",
					key, beforeTheHold, got)
			}
		}
	})
}
