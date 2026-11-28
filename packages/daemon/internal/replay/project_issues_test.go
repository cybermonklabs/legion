package replay

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// unhandedIssue is the events of ACME-9, an issue of the window's project that collect reads only
// because the listing of the project's issues names it: no daemon line names it, it stands in no
// tree and no one handed it to Legion. It is filed todo with a rank above every issue of the
// window, and a question is asked of a person on it, which the approvals signal reads.
var unhandedIssue = []json.RawMessage{
	json.RawMessage(`{"id":901,"issue_key":"ACME-9","artifact_id":null,"project":"ACME","seq":1,"type":"issue.created",` +
		`"actor":{"kind":"user","id":"acme-person"},"notify":true,"created_at":"2001-03-05T09:30:00Z","payload":{"key":"ACME-9","number":9,` +
		`"project":"ACME","title":"An issue no one handed to Legion","status":"todo","labels":[],"parent":null,"blocked_by":[],"rank":"0",` +
		`"priority":0,"assignee":null,"repo":null,"resolved_repo":"acme/widgets","changed":["title","status","rank","priority"]}}`),
	json.RawMessage(`{"id":902,"issue_key":"ACME-9","artifact_id":null,"project":"ACME","seq":2,"type":"ask.opened",` +
		`"actor":{"kind":"user","id":"acme-person"},"notify":true,"created_at":"2001-03-05T10:00:00Z","payload":{"id":"00000000-0000-4000-8000-0000000000f9",` +
		`"kind":"question","state":"open","issue_key":"ACME-9","asked_of":"acme-owner","question":"Should this ship?"}}`),
}

// A collect reads the events of every issue of the project (the approvals signal reads every ask
// of it), so a window now holds issues no daemon line names and no one handed to Legion. Such an
// issue moves no replay: never-starts, with ACME-9 (unhandedIssue: todo, a P0 ranked above every
// other issue, with a question asked of a person) beside its own issues, reads and writes with
// ACME-9's events held for the approvals signal, its trace holds ACME-9 with no entry, no visit and
// no slot, and its replay under each rule and never-started scenario expect.json names gives the
// scorecard the window gives without ACME-9 and the same admission lines, in the same order.
func TestAnIssueReadOnlyForItsEventsMovesNoReplay(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	without, expect := readFixture(t, "never-starts")
	with := without
	with.Events = maps.Clone(without.Events)
	with.Events["ACME-9"] = unhandedIssue
	dir := filepath.Join(t.TempDir(), "with")
	if err := with.Write(dir); err != nil {
		t.Fatal(err)
	}
	read, err := ReadWindow(dir)
	if err != nil {
		t.Fatalf("ReadWindow of the window with ACME-9: %v", err)
	}
	if !reflect.DeepEqual(read.Events["ACME-9"], unhandedIssue) {
		t.Errorf("the window holds ACME-9's events as %s, want %s", read.Events["ACME-9"], unhandedIssue)
	}
	trace, err := BuildTrace(read)
	if err != nil {
		t.Fatalf("BuildTrace: %v", err)
	}
	if iss := trace.Issues["ACME-9"]; iss == nil || len(iss.Entries) != 0 || len(iss.Visits) != 0 || len(iss.Slots) != 0 {
		t.Errorf("the trace reads ACME-9 as %+v, want it with no entry, no visit and no slot", iss)
	}

	runs, err := expect.runs()
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		t.Run(run.name, func(t *testing.T) {
			t.Parallel()
			replay := func(w Window) (Scorecard, []string) {
				out := t.TempDir()
				result, err := Run(context.Background(), RunConfig{Window: w, Rule: replayedRule(t, run.rule), Branch: run.branch,
					Seed: run.want.Seed, Slowdown: cmp.Or(run.want.Slowdown, 1), PGDSN: dsn, Out: out})
				if err != nil {
					t.Fatalf("the replay under %s on %s failed: %v", run.rule, run.branch.Name, err)
				}
				var admissions []string
				data, err := os.ReadFile(filepath.Join(out, logFile))
				if err != nil {
					t.Fatal(err)
				}
				for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					var head struct {
						Msg string `json:"msg"`
					}
					if err := json.Unmarshal([]byte(line), &head); err != nil {
						t.Fatalf("a line of the replay's log does not read: %v", err)
					}
					if strings.HasPrefix(head.Msg, "admission") {
						admissions = append(admissions, line)
					}
				}
				return result.Scorecard, admissions
			}
			wantScore, wantAdmissions := replay(without)
			gotScore, gotAdmissions := replay(read)
			for _, diff := range scorecardDiffs(gotScore, wantScore) {
				t.Errorf("with ACME-9 beside its issues: %s", diff)
			}
			if !slices.Equal(gotAdmissions, wantAdmissions) {
				t.Errorf("with ACME-9 the replay writes the admission lines\n  %s\nwithout it\n  %s", strings.Join(gotAdmissions, "\n  "),
					strings.Join(wantAdmissions, "\n  "))
			}
		})
	}
}
