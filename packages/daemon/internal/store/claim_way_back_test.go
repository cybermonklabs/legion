package store

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// planStepAgentMigrationFile is the migration whose header carries the way back of the release that
// names each step's agent.
const planStepAgentMigrationFile = "20261124213820_plan_step_agent.up.sql"

// headerWayBackOf is the transaction the header of the embedded migration file carries between its
// psql heredoc and the heredoc's terminator, with the comment marker and the opener's indent taken
// off each line: what an operator runs at the rollback. It fails the test where the header holds
// no such block, so the test never runs an empty way back and calls it clean.
func headerWayBackOf(t *testing.T, file string) string {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	header := ""
	for _, migration := range all {
		if migration.Name == file {
			header = migration.SQL
		}
	}
	if header == "" {
		t.Fatalf("the embedded migrations hold no %s", file)
	}
	const opener, terminator = "PGPASSFILE=<pgpass file> psql '<postgres_url>' -X -v ON_ERROR_STOP=1 <<'SQL'", "SQL"
	var lines []string
	indent, open := "", false
	for _, line := range strings.Split(header, "\n") {
		comment, isComment := strings.CutPrefix(line, "--")
		if !isComment {
			break
		}
		if !open {
			if strings.TrimSpace(comment) == opener {
				indent, open = comment[:len(comment)-len(strings.TrimLeft(comment, " "))], true
			}
			continue
		}
		text := strings.TrimPrefix(comment, indent)
		if text == terminator {
			return strings.Join(lines, "\n") + "\n"
		}
		lines = append(lines, text)
	}
	t.Fatalf("the header of %s carries no way back between %q and %q", file, opener, terminator)
	return ""
}

// addedFields are the fields this release added to the outbox payloads the release before decodes:
// the claim a row acts on (record.SuperviseRequest.Claim and its kin), the pod whose refusal queued
// a start (record.SuperviseRequest.RefusedPod) and the session a start leaves
// (record.SuperviseRequest.LeaveSession).
var addedFields = []string{"Claim", "RefusedPod", "LeaveSession"}

// decodeAsTheReleaseBefore decodes payload as the release before this one decodes a row of its
// kind: strictly (record.DecodeOutboxPayload's DisallowUnknownFields), into the payload's type as
// it stood then, which is shape's type without the fields this release added to it (addedFields).
func decodeAsTheReleaseBefore(payload []byte, shape any) error {
	of := reflect.TypeOf(shape)
	var fields []reflect.StructField
	for i := range of.NumField() {
		if field := of.Field(i); !slices.Contains(addedFields, field.Name) {
			fields = append(fields, field)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	return decoder.Decode(reflect.New(reflect.StructOf(fields)).Interface())
}

// A row this release queues can name the claim it acts on: a supervise request, a release's
// workspace save, an update round's ask, a hook's prompt, a merge forward's message and a
// provider-errors controller notice. The release before knows no such key
// and decodes a payload strictly, so such a row left in place at a rollback fails its decode at
// every attempt. The way back the migration's header
// gives takes the key off each queued row of those kinds and leaves every other key as it was, and
// the release before then decodes the row; this release still decodes it after the roll forward,
// with no claim named.
func TestTheWayBackTakesTheClaimOffEachRowTheReleaseBeforeDecodes(t *testing.T) {
	ctx := context.Background()
	st := emptyStore(t)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const keyed = `"claim":"legion-legion-legion-208-implementer"`
	rows := []struct {
		kind, payload string
		shape         any
	}{
		{kind: "supervise", shape: record.SuperviseRequest{},
			payload: `{"op":"start","tree":"LEGION-208","role":"tester",` + keyed + `,"generation":1,"phase":"testing","entry":2,"task":"Test it."}`},
		{kind: "issue_bundle", shape: record.IssueWorkspaceCheck{},
			payload: `{"tree":"LEGION-208","generation":1,"treeGeneration":1,"role":"tester",` + keyed + `}`},
		{kind: "update_ask", shape: record.UpdateAsk{},
			payload: `{"role":"tester",` + keyed + `,"round":"pod:9d3f0c7a","pod":"9d3f0c7a","text":"Push your work."}`},
		{kind: "hook_prompt", shape: record.HookPrompt{},
			payload: `{"hook":"back-to-implementing","role":"implementer",` + keyed + `,"from":"reviewing","to":"implementing","count":1,"counts":{"reviewing-implementing":1},"prompt":"Read the thread."}`},
		{kind: "merge_forward", shape: record.MergeForwardMessage{},
			payload: `{"role":"implementer",` + keyed + `,"step":"ask","head":"c0de","base":"main","phase":"testing","pullRequest":42,"why":"required-check-not-green"}`},
		{kind: "controller_notice", shape: record.ControllerNotice{},
			payload: `{"kind":"provider-errors","role":"tester",` + keyed + `,"reason":"6 turns in a row ended in a provider error"}`},
	}
	ids := make([]int64, len(rows))
	for i, row := range rows {
		if err := decodeAsTheReleaseBefore([]byte(row.payload), row.shape); err == nil || !strings.Contains(err.Error(), `unknown field "claim"`) {
			t.Fatalf("the release before decoding a %s row that names its claim = %v, want the strict decode's refusal of the key", row.kind, err)
		}
		if err := st.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
			values ($1, 'LEGION-208', $2, 0, now(), '', '') returning id`, row.kind, row.payload).Scan(&ids[i]); err != nil {
			t.Fatalf("queue the %s row: %v", row.kind, err)
		}
	}

	if _, err := st.Pool().Exec(ctx, headerWayBackOf(t, planStepAgentMigrationFile), pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("run the way back as one transaction: %v", err)
	}

	for i, row := range rows {
		var payload []byte
		if err := st.Pool().QueryRow(ctx, `select payload from outbox where id = $1`, ids[i]).Scan(&payload); err != nil {
			t.Fatalf("read the %s row after the way back: %v", row.kind, err)
		}
		if err := decodeAsTheReleaseBefore(payload, row.shape); err != nil {
			t.Fatalf("the release before decoding the %s row after the way back = %v, want it decoded: %s", row.kind, err, payload)
		}
		var before, after map[string]any
		if err := json.Unmarshal([]byte(row.payload), &before); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(payload, &after); err != nil {
			t.Fatal(err)
		}
		delete(before, "claim")
		if !maps.EqualFunc(before, after, func(a, b any) bool { return reflect.DeepEqual(a, b) }) {
			t.Fatalf("the %s row after the way back = %v, want %v: the claim taken off and every other key kept", row.kind, after, before)
		}
		decoded, err := record.DecodeOutboxPayload(record.OutboxRow{ID: ids[i], Kind: record.OutboxKind(row.kind), Payload: payload})
		if err != nil {
			t.Fatalf("this release decoding the %s row after the way back: %v", row.kind, err)
		}
		if named := reflect.ValueOf(decoded).FieldByName("Claim").String(); named != "" {
			t.Fatalf("this release reads the %s row after the way back as naming %q, want no claim", row.kind, named)
		}
	}
}

// A start a refused launch queues names the pod whose refusal queued it
// (record.SuperviseRequest.RefusedPod), and a start of a step whose agent took a name a key held
// names the session to leave (record.SuperviseRequest.LeaveSession): keys the release before does
// not know and decodes strictly, so such a start left queued at a rollback would fail its decode at
// every attempt. The way back the migration's header gives takes each key off each queued supervise
// row, as it takes `claim`, and keeps every other key: the release before then decodes the row, and
// this release still decodes it after the roll forward, naming neither.
func TestTheWayBackTakesTheRefusedPodAndTheSessionToLeaveOffAQueuedStart(t *testing.T) {
	for _, key := range []string{"refusedPod", "leaveSession"} {
		t.Run(key, func(t *testing.T) {
			ctx := context.Background()
			st := emptyStore(t)
			if _, err := st.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			queued := `{"op":"start","tree":"LEGION-208","role":"tester","generation":1,"` + key + `":"7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11"}`
			if err := decodeAsTheReleaseBefore([]byte(queued), record.SuperviseRequest{}); err == nil || !strings.Contains(err.Error(), `unknown field "`+key+`"`) {
				t.Fatalf("the release before decoding a start that names %s = %v, want the strict decode's refusal of the key", key, err)
			}
			var id int64
			if err := st.Pool().QueryRow(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
				values ('supervise', 'LEGION-208', $1, 0, now(), '', '') returning id`, queued).Scan(&id); err != nil {
				t.Fatalf("queue the start: %v", err)
			}

			if _, err := st.Pool().Exec(ctx, headerWayBackOf(t, planStepAgentMigrationFile), pgx.QueryExecModeSimpleProtocol); err != nil {
				t.Fatalf("run the way back as one transaction: %v", err)
			}

			var payload []byte
			if err := st.Pool().QueryRow(ctx, `select payload from outbox where id = $1`, id).Scan(&payload); err != nil {
				t.Fatalf("read the start after the way back: %v", err)
			}
			if err := decodeAsTheReleaseBefore(payload, record.SuperviseRequest{}); err != nil {
				t.Fatalf("the release before decoding the start after the way back = %v, want it decoded: %s", err, payload)
			}
			var before, after map[string]any
			if err := json.Unmarshal([]byte(queued), &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(payload, &after); err != nil {
				t.Fatal(err)
			}
			delete(before, key)
			if !maps.EqualFunc(before, after, func(a, b any) bool { return reflect.DeepEqual(a, b) }) {
				t.Fatalf("the start after the way back = %v, want %v: %s taken off and every other key kept", after, before, key)
			}
			decoded, err := record.DecodeOutboxPayload(record.OutboxRow{ID: id, Kind: record.OutboxKindSupervise, Payload: payload})
			if err != nil {
				t.Fatalf("this release decoding the start after the way back: %v", err)
			}
			if start, ok := decoded.(record.SuperviseRequest); !ok || start.RefusedPod != "" || start.LeaveSession != "" {
				t.Fatalf("this release reads the start after the way back as %+v, want a start naming neither key", decoded)
			}
		})
	}
}
