package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/plan"
)

// TestRigPlanCheck is the label's own code for the skill-scenario rig's plan check
// (packages/pi-legion/scripts/skill-scenarios/label-texts.sh plan-check): the answer of this
// label's own register_plan route to a plan version as the run's Dispatch holds it. The route
// answers for planHarness's root, given the request's blocks as the request's version of the
// request's document, by that root's architect. With RIG_TEXTS_REQUEST and RIG_TEXTS_ANSWER it
// reads the plan request (scored-runs-contracts.ts, PlanRequest) and writes the plan answer; with
// neither it runs its own cases.
func TestRigPlanCheck(t *testing.T) {
	request, answer := os.Getenv("RIG_TEXTS_REQUEST"), os.Getenv("RIG_TEXTS_ANSWER")
	if (request == "") != (answer == "") {
		t.Fatalf("RIG_TEXTS_REQUEST is %q and RIG_TEXTS_ANSWER is %q: give both or neither", request, answer)
	}
	if request != "" {
		data, err := os.ReadFile(request)
		if err != nil {
			t.Fatal(err)
		}
		req, err := decodeRigPlanRequest(data)
		if err != nil {
			t.Fatalf("the plan request %s: %v", request, err)
		}
		encoded, err := json.MarshalIndent(rigPlanCheck(t, req), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(answer, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	task := func(id, steps string) rigPlanBlock {
		return rigPlanBlock{ID: id, Type: plan.TaskBlockType, Attributes: map[string]string{"steps": steps}, Text: "Build the rig's widget."}
	}
	for _, tc := range []struct {
		name   string
		blocks []rigPlanBlock
		want   int
		holds  string
	}{
		{"a plan of one task is taken", []rigPlanBlock{task("rig-a", "implementer reviewer")}, http.StatusOK, `"id":"rig-a"`},
		// A step of a role neither the deployment nor the plan defines is refused at every label
		// this entry builds at, where a plan of two tasks is not: a label that runs several tasks
		// of one plan takes one.
		{"a plan whose step names a role nobody defines is refused", []rigPlanBlock{task("rig-a", "implementer rig-prober")}, http.StatusConflict, "rig-prober"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// register_plan takes a document by its Dispatch id, a UUID.
			got := rigPlanCheck(t, rigPlanRequest{ArtifactID: "5d1e0c3e-2b7d-4e5a-9c81-3d4f5e6a7b80", Version: 2, LatestVersion: 2, Blocks: tc.blocks})
			if got.Status != tc.want || !strings.Contains(got.Body, tc.holds) {
				t.Fatalf("register_plan answered %d %s, want %d with a body holding %s", got.Status, got.Body, tc.want, tc.holds)
			}
		})
	}
}

// rigPlanRequest is the request of label-texts.sh plan-check (PlanRequest in
// packages/pi-legion/scripts/skill-scenarios/scored-runs-contracts.ts).
type rigPlanRequest struct {
	ArtifactID    string         `json:"artifactId"`
	Version       int            `json:"version"`
	LatestVersion int            `json:"latestVersion"`
	Blocks        []rigPlanBlock `json:"blocks"`
}

type rigPlanBlock struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Attributes map[string]string `json:"attributes"`
	Text       string            `json:"text"`
}

// rigPlanAnswer is the answer of label-texts.sh plan-check (PlanAnswer): the route's status and
// its body as text.
type rigPlanAnswer struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// decodeRigPlanRequest reads a plan request, refusing a field this test does not handle by its
// name.
func decodeRigPlanRequest(data []byte) (rigPlanRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var req rigPlanRequest
	if err := decoder.Decode(&req); err != nil {
		return rigPlanRequest{}, err
	}
	if decoder.More() {
		return rigPlanRequest{}, errors.New("the request holds more than one JSON value")
	}
	if req.ArtifactID == "" || req.Version < 1 || req.LatestVersion < 1 {
		return rigPlanRequest{}, errors.New("the request names no artifactId, version or latestVersion")
	}
	return req, nil
}

// rigPlanCheck gives req's blocks to planHarness's Dispatch as version req.Version of the document
// req.ArtifactID of the harness's root, whose latest version is req.LatestVersion, and answers
// what register_plan answers its architect for that version.
func rigPlanCheck(t *testing.T, req rigPlanRequest) rigPlanAnswer {
	t.Helper()
	h, _, statuses := planHarness(t)
	const root = "LEGION-208"
	statuses.documents[req.ArtifactID] = root
	statuses.approvals[req.ArtifactID] = dispatch.Approval{State: "none", LatestVersion: req.LatestVersion}
	blocks := make([]dispatch.DocumentBlock, 0, len(req.Blocks))
	for _, block := range req.Blocks {
		blocks = append(blocks, dispatch.DocumentBlock{ID: block.ID, Type: block.Type, Attributes: block.Attributes, Text: block.Text})
	}
	statuses.givePlan(req.ArtifactID, req.Version, blocks...)
	architect := newLiveClaim(t, h, root, claim.RoleArchitect)
	recorder := registerPlan(h, architect.grant(t), root, req.ArtifactID, req.Version)
	return rigPlanAnswer{Status: recorder.Code, Body: recorder.Body.String()}
}
