package record

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// roleMessageBefore is a role message as the release before R4 of dispatch://INFRA-4480 decodes
// it: Role and Body alone, read strictly (decodeOutboxJSON's DisallowUnknownFields on that release).
type roleMessageBefore struct {
	Role string `json:"role"`
	Body string `json:"body"`
}

// decodeBefore decodes payload as the release before R4 does.
func decodeBefore(payload []byte) (roleMessageBefore, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var message roleMessageBefore
	err := decoder.Decode(&message)
	return message, err
}

// A rollback to the release before R4 of dispatch://INFRA-4480 runs RoleMessageGateWayBack with the
// daemon stopped (docs/kubernetes.md, "Rolling back the release that tells a released holder the
// design gate's move"). A queued role message that carries a design gate's move fails that
// release's strict decode before the statement, at every attempt on its backoff, and decodes with
// its role and body unchanged after it; a role message with no move is left as it is.
//
// At 1103560a the new key had no way back: no such statement existed, and a rollback left every
// marked row failing that release's decode.
func TestARoleMessageOfAGateMoveDecodesOnTheReleaseBeforeAfterItsWayBack(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC()
	marked := RoleMessage{Role: "legion-legion-legion-801-tester", Body: "The design gate of LEGION-800 closed.",
		Gate: &GateMove{Tree: "LEGION-800", Step: DesignStepSpec, Version: 2, Seq: 3}}
	plain := RoleMessage{Role: "legion-legion-legion-801-tester", Body: "Pull request #42: CI settled at head1."}
	var markedID, plainID int64
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issueFixture("LEGION-801")))
		for _, msg := range []struct {
			payload RoleMessage
			id      *int64
		}{{marked, &markedID}, {plain, &plainID}} {
			row, err := NewOutboxRow("LEGION-801", msg.payload, now)
			must(t, err)
			id, err := records.EnqueueID(ctx, tx, row)
			must(t, err)
			*msg.id = id
		}
	})
	payload := func(id int64) []byte {
		t.Helper()
		var raw []byte
		if err := st.Pool().QueryRow(ctx, `select payload from outbox where id = $1`, id).Scan(&raw); err != nil {
			t.Fatalf("read outbox row %d: %v", id, err)
		}
		return raw
	}

	if _, err := decodeBefore(payload(markedID)); err == nil || !strings.Contains(err.Error(), `unknown field "gate"`) {
		t.Fatalf("the marked row decoded on the release before as %v, want it refused for its gate key: the case the way back exists for", err)
	}
	if _, err := st.Pool().Exec(ctx, RoleMessageGateWayBack); err != nil {
		t.Fatalf("run the way back: %v", err)
	}
	for _, tc := range []struct {
		id   int64
		want RoleMessage
	}{{markedID, marked}, {plainID, plain}} {
		got, err := decodeBefore(payload(tc.id))
		if err != nil || got.Role != tc.want.Role || got.Body != tc.want.Body {
			t.Fatalf("row %d after the way back decodes on the release before as %+v, %v; want role %q and body %q", tc.id, got, err, tc.want.Role, tc.want.Body)
		}
	}
}
