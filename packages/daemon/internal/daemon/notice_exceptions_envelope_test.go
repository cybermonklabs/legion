package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// rehold reads the listener's exception envelope with encoding/json into the three members it
// needs (event_id, source and payload), so the `role` field an envelope may carry (the role a
// message Dispatch's API sent for a route to a role was sent for) is ignored: each report is
// answered as the same report without the field. No row reaches the outbox's database: a report
// from another source and one naming a reason this daemon does not know are refused naming the
// event, and a report of a publish the listener refused for want of a holder is logged at debug.
func TestReholdReadsAnExceptionEnvelopeThatCarriesTheRoleFieldAsWithoutIt(t *testing.T) {
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect}
	topic := architectTopic(t, "LEGION-1")
	report := func(reason string) laneReport {
		return laneReport{"evt-role-field", reason, topic, "pr-blocked on LEGION-2", notice, "legion-outbox:7", "ses_live"}
	}
	for _, tc := range []struct {
		name    string
		report  laneReport
		source  string // the envelope's source in place of the listener's own, when set
		wantErr string
		wantLog string
	}{
		{name: "a report from another source", report: report("delivery_failed"), source: "agent", wantErr: `envelope "evt-role-field" from source "agent"`},
		{name: "a reason this daemon does not know", report: report("a_later_reason"), wantErr: `exception evt-role-field names reason "a_later_reason"`},
		{name: "a publish the listener refused for want of a holder", report: report("unclaimed"), wantLog: "outbox notice not re-held: the listener refused its publish"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := func(withRole bool) (refusal, logged string) {
				var lines bytes.Buffer
				log := slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{
					Level: slog.LevelDebug,
					// The time differs between two reads; everything else the line says must not.
					ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
						if attr.Key == slog.TimeKey && len(groups) == 0 {
							return slog.Attr{}
						}
						return attr
					},
				}))
				runner := &outbox{log: log, dispatchProject: "LEGION", records: record.NewStore(), supervisor: sup, project: "legion", now: time.Now}
				var envelope map[string]any
				if err := json.Unmarshal(tc.report.envelope(t), &envelope); err != nil {
					t.Fatalf("read the report's envelope: %v", err)
				}
				if tc.source != "" {
					envelope["source"] = tc.source
				}
				if withRole {
					envelope["role"] = "legion-legion-legion-1-architect"
				}
				data, err := json.Marshal(envelope)
				if err != nil {
					t.Fatalf("encode the report's envelope: %v", err)
				}
				if err := runner.rehold(context.Background(), data); err != nil {
					refusal = err.Error()
				}
				return refusal, lines.String()
			}

			refusal, logged := read(false)
			if !strings.Contains(refusal, tc.wantErr) || (tc.wantErr == "" && refusal != "") {
				t.Fatalf("rehold of the envelope without the field = %q, want a refusal naming %q", refusal, tc.wantErr)
			}
			if !strings.Contains(logged, tc.wantLog) {
				t.Fatalf("rehold of the envelope without the field logged %q, want %q", logged, tc.wantLog)
			}
			refusalWithRole, loggedWithRole := read(true)
			if refusalWithRole != refusal || loggedWithRole != logged {
				t.Fatalf("rehold of the envelope with the field = %q, logging %q; want %q, logging %q, as without it", refusalWithRole, loggedWithRole, refusal, logged)
			}
		})
	}
}
