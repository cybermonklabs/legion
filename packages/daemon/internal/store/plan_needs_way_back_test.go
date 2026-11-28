package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// planNeedsWayBackExtract is the committed copy of the way back docs/kubernetes.md gives under
// "Rolling back the release that records each task's needs". internal/textcheck's
// TestThePlanNeedsWayBackExtractIsTheGuidesWayBackBlock holds it to the guide's block, so this
// file reads the copy and not the guide.
const planNeedsWayBackExtract = "plan_needs_way_back_extract.txt"

// planNeedsWayBackFor is the copy's statement as an operator who rolls project back runs it: the
// copy's body under its `#` header, with project's key in place of rollingBackProject.
func planNeedsWayBackFor(t *testing.T, project string) string {
	t.Helper()
	text, err := os.ReadFile(planNeedsWayBackExtract)
	if err != nil {
		t.Fatalf("read %s: %v", planNeedsWayBackExtract, err)
	}
	body := string(text)
	for strings.HasPrefix(body, "#") {
		_, body, _ = strings.Cut(body, "\n")
	}
	body = strings.Trim(body, "\n")
	if !strings.Contains(body, rollingBackProject) {
		t.Fatalf("%s holds no %s to name the project by: %q", planNeedsWayBackExtract, rollingBackProject, body)
	}
	return strings.ReplaceAll(body, rollingBackProject, project)
}

// planRecordBeforeTheNeeds is record.PlanRecord as the release before this one declares it: every
// field of this release's shape but each task's Needs. It is a copy because that release's type
// is not in the tree. That release decodes a plan_record row strictly
// (record.DecodeOutboxPayload, DisallowUnknownFields), so a row this shape refuses is a row it
// fails at every attempt.
type planRecordBeforeTheNeeds struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
	Source     string `json:"source"`
	*PlanDocumentBeforeTheNeeds
	Tasks []planRecordTaskBeforeTheNeeds `json:"tasks"`
}

// PlanDocumentBeforeTheNeeds is that release's record.PlanDocument, embedded as the release
// embeds it. It is exported because encoding/json cannot set an embedded pointer to an unexported
// type.
type PlanDocumentBeforeTheNeeds struct {
	ArtifactID string `json:"artifact_id"`
	Version    int    `json:"version"`
}

type planRecordTaskBeforeTheNeeds struct {
	ID    string   `json:"id"`
	Steps []string `json:"steps"`
}

// decodedBeforeTheNeeds is the release before's strict decode of a plan_record payload: the plan
// it reads, and the error where it refuses the payload.
func decodedBeforeTheNeeds(payload []byte) (planRecordBeforeTheNeeds, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var plan planRecordBeforeTheNeeds
	err := decoder.Decode(&plan)
	return plan, err
}

// The way back of the release that records each task's needs takes `needs` off each task of each
// queued plan_record row of the project that rolls back, so the release before decodes each such
// row and sends its plan, the tasks and their steps as this release queued them. The rows here are
// this release's (record.NewOutboxRow): an owner's plan of two tasks, one of which needs the other,
// and a default plan whose one task needs none, of LEGION; and a plan of OTHER, whose daemon stays
// on this release. Before the statement each LEGION row fails the release before's decode, which
// is the fault the way back answers. After it, each LEGION row decodes to the plan queued, and
// OTHER's row still carries its needs. A second run changes nothing.
func TestThePlanNeedsWayBackLetsTheReleaseBeforeDecodeItsProjectsPlanRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 11, 21, 22, 0, 0, 0, time.UTC)
	steps := []string{"planner", "implementer", "tester", "reviewer", "acceptance:owner"}
	queued := []struct {
		issue string
		plan  record.PlanRecord
	}{
		{issue: "LEGION-1", plan: record.PlanRecord{ID: "4d5e6f70-8192-5cad-9e0f-3a4b5c6d7e8f", Generation: 2, Source: "owner",
			PlanDocument: &record.PlanDocument{ArtifactID: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", Version: 3},
			Tasks: []record.PlanRecordTask{
				{ID: "ui", Steps: []string{"tester", "implementer"}, Needs: &[]string{"api"}},
				{ID: "api", Steps: []string{"implementer", "reviewer"}, Needs: &[]string{}},
			}}},
		{issue: "LEGION-2", plan: record.PlanRecord{ID: "5e6f7081-92a3-5bde-8f01-4b5c6d7e8f90", Generation: 1, Source: "default",
			Tasks: []record.PlanRecordTask{{ID: "default", Steps: steps, Needs: &[]string{}}}}},
		{issue: "OTHER-7", plan: record.PlanRecord{ID: "6e7f8091-a2b3-5c4d-9e5f-6a7b8c9d0e1f", Generation: 1, Source: "default",
			Tasks: []record.PlanRecordTask{{ID: "default", Steps: steps, Needs: &[]string{}}}}},
	}
	store := emptyStore(t)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, q := range queued {
		row, err := record.NewOutboxRow(q.issue, q.plan, at)
		if err != nil {
			t.Fatalf("queue the plan of %s: %v", q.issue, err)
		}
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, $2, $3, 0, $4, '')`, string(row.Kind), row.Issue, row.Payload, row.NextAt); err != nil {
			t.Fatalf("insert the plan of %s: %v", q.issue, err)
		}
	}
	payloads := func() map[string][]byte {
		t.Helper()
		rows, err := store.Pool().Query(ctx, `select issue, payload from outbox where kind = 'plan_record' order by id`)
		if err != nil {
			t.Fatalf("read the plan rows: %v", err)
		}
		defer rows.Close()
		read := map[string][]byte{}
		for rows.Next() {
			var issue string
			var payload []byte
			if err := rows.Scan(&issue, &payload); err != nil {
				t.Fatalf("scan a plan row: %v", err)
			}
			read[issue] = payload
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the plan rows: %v", err)
		}
		if len(read) != len(queued) {
			t.Fatalf("the outbox holds plan rows of %d issues, want %d: nothing to compare", len(read), len(queued))
		}
		return read
	}

	for issue, payload := range payloads() {
		if _, err := decodedBeforeTheNeeds(payload); err == nil {
			t.Fatalf("the release before decodes the plan row of %s before the way back: %s; want it refused, or this test cannot see the fault", issue, payload)
		}
	}

	for run := 1; run <= 2; run++ {
		if _, err := store.Pool().Exec(ctx, planNeedsWayBackFor(t, "LEGION"), pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("run %d of the way back: %v", run, err)
		}
		after := payloads()
		for _, q := range queued {
			decoded, err := decodedBeforeTheNeeds(after[q.issue])
			if q.issue == "OTHER-7" {
				if err == nil {
					t.Errorf("run %d: the way back for LEGION took the needs off OTHER's plan row too: %s", run, after[q.issue])
				}
				continue
			}
			if err != nil {
				t.Fatalf("run %d: the release before refuses the plan row of %s after the way back: %v\n%s", run, q.issue, err, after[q.issue])
			}
			want := planRecordBeforeTheNeeds{ID: q.plan.ID, Generation: q.plan.Generation, Source: q.plan.Source}
			if q.plan.PlanDocument != nil {
				want.PlanDocumentBeforeTheNeeds = &PlanDocumentBeforeTheNeeds{ArtifactID: q.plan.ArtifactID, Version: q.plan.Version}
			}
			for _, task := range q.plan.Tasks {
				want.Tasks = append(want.Tasks, planRecordTaskBeforeTheNeeds{ID: task.ID, Steps: task.Steps})
			}
			if !reflect.DeepEqual(decoded, want) {
				t.Errorf("run %d: the release before reads the plan row of %s as\n%+v\nwant the plan queued, its tasks in their order\n%+v", run, q.issue, decoded, want)
			}
		}
	}
}
