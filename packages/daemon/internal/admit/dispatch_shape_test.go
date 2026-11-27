package admit

//ci:reads packages/daemon/internal/intake/testdata/dispatch-resolved/* TestTheDispatchShapeReachesTheRecordFromBytes and TestAnOlderDispatchsPayloadPromotesNothing read the event logs through ../intake/testdata
//ci:reads packages/daemon/internal/dispatch/testdata/resolved/* TestTheDispatchShapeReachesTheRecordFromBytes reads the listing, detail and PATCH bodies through ../dispatch/testdata

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The fixtures of Dispatch's own bytes reach the record through the paths the daemon reads them
// on: the issue event logs of internal/intake's testdata/dispatch-resolved and the HTTP bodies of
// internal/dispatch's testdata/resolved, each directory's README saying which file is a capture of
// an older Dispatch and which is written by hand in the shape dispatch://INFRA-5793/spec#b-j-inherit
// and #b-j-events give. Their issues are of the project ACME, so these tests record under it.

// shapeProject is the Dispatch project the fixtures' issues are of.
const shapeProject = "ACME"

// shapeEvents is the event log ../intake/testdata/dispatch-resolved/<name> holds, one raw event for
// each element.
func shapeEvents(t *testing.T, name string) []json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "intake", "testdata", "dispatch-resolved", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var events []json.RawMessage
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return events
}

// shapeUpdate is the first issue.updated of the log name that leaves its issue in todo, as it
// stands on the wire.
func shapeUpdate(t *testing.T, name string) []byte {
	t.Helper()
	for _, raw := range shapeEvents(t, name) {
		var event struct {
			Type    string `json:"type"`
			Payload struct {
				Status string `json:"status"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("read an event of %s: %v", name, err)
		}
		if event.Type == "issue.updated" && event.Payload.Status == "todo" {
			return raw
		}
	}
	t.Fatalf("%s holds no issue.updated that leaves its issue in todo", name)
	return nil
}

// decodedShapeEvent is raw, an event of shapeProject's issue key, through the daemon's own decoder
// (intake.DecodeDispatchEvent), which must read it whole.
func decodedShapeEvent(t *testing.T, key string, raw []byte) intake.DispatchIssue {
	t.Helper()
	fact, unread, err := intake.DecodeDispatchEvent(shapeProject, key, raw)
	if err != nil || len(unread) != 0 {
		t.Fatalf("decode the event of %s: %v, unread %q", key, err, unread)
	}
	issue, ok := fact.(intake.DispatchIssue)
	if !ok {
		t.Fatalf("the event of %s decoded to %#v, want a Dispatch issue", key, fact)
	}
	return issue
}

// shapeBody is ../dispatch/testdata/resolved/<name>, with each pair of change replacing its first
// string by its second everywhere, every one of which the body must hold.
func shapeBody(t *testing.T, name string, change ...[2]string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "dispatch", "testdata", "resolved", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	body := string(data)
	for _, c := range change {
		if !strings.Contains(body, c[0]) {
			t.Fatalf("%s holds no %s to change", name, c[0])
		}
		body = strings.ReplaceAll(body, c[0], c[1])
	}
	return []byte(body)
}

// servingDispatch is the daemon's client of a stand-in Dispatch that answers every request with
// body, as the real Dispatch answers the read or write the client makes.
func servingDispatch(t *testing.T, body []byte) *dispatch.HTTPClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return dispatch.New(server.URL, dispatch.BearerTransport("dispatch-test-token", nil))
}

// shapeAdmission is admission of shapeProject at cap, under the design gate quiet's admission runs.
func shapeAdmission(t *testing.T, cap int) *Admission {
	t.Helper()
	return newAdmissionForProject(t, shapeProject, cap, config.DesignGate("").Steps(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// shapeRoot records root as the running root of its own tree in shapeProject at its own priority,
// as p0Tree records a tree whose design gate is approved: its lifecycle open and the root in
// accepting, which holds no slot.
func shapeRoot(t *testing.T, pool *pgxpool.Pool, root string, priority record.Priority) {
	t.Helper()
	putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: shapeProject,
		Title: root, Tree: root, Phase: phase.Accepting, Generation: 1, Status: "needs_review", DispatchStatus: "needs_review", Rank: "B",
		HandedOver: true, Priority: priority})
	seedLifecycleForProject(t, pool, shapeProject, root)
	seedGate(t, pool, record.DesignGate{Issue: root, ArtifactID: "artifact-" + root, LatestVersion: 1, ApprovedVersion: new(1),
		ApprovedMark: record.EventMark{Seq: 4, At: fixedNow.Add(-time.Hour)}})
}

// shapeMember records key under root's tree in shapeProject, at admitted, standing at status as
// Dispatch last showed it at seq, with the own and resolved priority given.
func shapeMember(t *testing.T, pool *pgxpool.Pool, root, key, status string, seq int64, own record.Priority, resolved record.Resolved) {
	t.Helper()
	parent := root
	putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: shapeProject,
		Title: key, Tree: root, Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: status, DispatchStatus: status, Rank: "B1",
		LastDispatchSeq: seq, Priority: own, Resolved: resolved})
}

// resolvedName is resolved as a failure names it, with the issue it comes from (`priority_from`),
// an unknown one and a known none apart.
func resolvedName(resolved record.Resolved) string {
	if !resolved.Known {
		return "unknown"
	}
	return priorityName(resolved.Priority) + " from " + strconv.Quote(resolved.From)
}

// Each fixture of Dispatch's bytes reaches the record through the real path the daemon reads it
// on, and the record stores the own priority, the resolved one and the issue it comes from
// (`priority_from`) as the bytes give them: an issue event through intake.DecodeDispatchEvent and
// the fact path; a boot-listing row through the real client's ListIssues, intake.ListedIssue and
// the boot's reconcile; a detail read through GetIssue and intake.ObservedIssue, which records an
// unrecorded child of a live tree (a view of a recorded issue applies nothing, intake's
// settleObserved); and the issue a status write's PATCH answers through SetStatusAt and
// intake.OwnStatusWrite. The issue is a child of a running tree's root. On every path but the
// detail read, which records the child from nothing, its record held another own priority and a
// known resolved P0 (P2 where the row's is P0), so a path that kept either reads it: the answer's
// present null replaces a P0, and so does the older capture's missing field. The capture's issue
// is closed, so the listing, the detail and the PATCH answer read it at the status each path
// records a running child at. The updates Dispatch appends on a child for a write above it (J3)
// reach the record one after the other.
func TestTheDispatchShapeReachesTheRecordFromBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		// log is the file of the event log, http the name the HTTP bodies share
		// (<read>-<http>.json).
		log, http string
		key, root string
		own       record.Priority
		want      record.Resolved
		// capture says the bodies are the older capture's, of a closed issue.
		capture bool
	}{
		{name: "the older capture", log: "acme-1549-after-0.json", http: "acme-1549", key: "ACME-1549", root: "ACME-1547", capture: true},
		{name: "resolved P0", log: "resolved-0.json", http: "resolved-0", key: "ACME-12", root: "ACME-7",
			want: record.Resolved{Priority: record.P0, From: "OTHER-5", Known: true}},
		{name: "resolved P1", log: "resolved-1.json", http: "resolved-1", key: "ACME-12", root: "ACME-7",
			want: record.Resolved{Priority: record.P1, From: "OTHER-5", Known: true}},
		{name: "resolved P2", log: "resolved-2.json", http: "resolved-2", key: "ACME-12", root: "ACME-7",
			want: record.Resolved{Priority: record.P2, From: "OTHER-5", Known: true}},
		{name: "resolved P3", log: "resolved-3.json", http: "resolved-3", key: "ACME-12", root: "ACME-7",
			want: record.Resolved{Priority: record.P3, From: "OTHER-5", Known: true}},
		{name: "resolved null", log: "resolved-null.json", http: "resolved-null", key: "ACME-12", root: "ACME-7", want: record.Resolved{Known: true}},
		{name: "its own priority", log: "resolved-own.json", http: "resolved-own", key: "ACME-12", root: "ACME-7", own: record.P2,
			want: record.Resolved{Priority: record.P2, Known: true}},
	} {
		before := record.Resolved{Priority: record.P0, From: "OTHER-9", Known: true}
		if tc.want == (record.Resolved{Priority: record.P0, From: "OTHER-5", Known: true}) {
			before.Priority = record.P2
		}
		var todo, running [][2]string
		if tc.capture {
			todo = [][2]string{{`"done"`, `"todo"`}}
			running = [][2]string{{`"done"`, `"in_progress"`}}
		}
		t.Run(tc.name, func(t *testing.T) {
			check := func(t *testing.T, pool *pgxpool.Pool, path string) {
				t.Helper()
				got := issue(t, pool, tc.key)
				if got.Priority != tc.own || got.Resolved != tc.want {
					t.Fatalf("%s stored own %s and resolved %s, want own %s and resolved %s", path,
						priorityName(got.Priority), resolvedName(got.Resolved), priorityName(tc.own), resolvedName(tc.want))
				}
			}
			t.Run("an issue event", func(t *testing.T) {
				pool := migratedPool(t)
				admission := shapeAdmission(t, 1)
				shapeRoot(t, pool, tc.root, record.P1)
				shapeMember(t, pool, tc.root, tc.key, "todo", 1, record.P3, before)
				apply(t, pool, admission, "event", decodedShapeEvent(t, tc.key, shapeUpdate(t, tc.log)), admission.engine)
				check(t, pool, "the event")
			})
			t.Run("a boot-listing row", func(t *testing.T) {
				pool := migratedPool(t)
				admission := shapeAdmission(t, 1)
				shapeRoot(t, pool, tc.root, record.P1)
				shapeMember(t, pool, tc.root, tc.key, "todo", 1, record.P3, before)
				listed, err := servingDispatch(t, shapeBody(t, "listing-"+tc.http+".json", todo...)).ListIssues(context.Background(), shapeProject, nil)
				if err != nil || len(listed) != 1 {
					t.Fatalf("list the issues: %d issues, %v; want the one row", len(listed), err)
				}
				reconcileWithPosition(t, pool, admission, listed, listed[0].LastSeq, listed[0].LastSeq, true)
				check(t, pool, "the listing")
			})
			t.Run("a detail read", func(t *testing.T) {
				pool := migratedPool(t)
				admission := shapeAdmission(t, 1)
				shapeRoot(t, pool, tc.root, record.P1)
				detail, err := servingDispatch(t, shapeBody(t, "detail-"+tc.http+".json", todo...)).GetIssue(context.Background(), tc.key)
				if err != nil {
					t.Fatalf("get %s: %v", tc.key, err)
				}
				applyFact(t, pool, admission, "detail", intake.ObservedIssue(detail))
				check(t, pool, "the detail")
			})
			t.Run("a status write's returned issue", func(t *testing.T) {
				pool := migratedPool(t)
				admission := shapeAdmission(t, 1)
				shapeRoot(t, pool, tc.root, record.P1)
				client := servingDispatch(t, shapeBody(t, "patch-"+tc.http+".json", running...))
				var answered struct {
					LastSeq int64 `json:"last_seq"`
				}
				if err := json.Unmarshal(shapeBody(t, "patch-"+tc.http+".json"), &answered); err != nil {
					t.Fatal(err)
				}
				shapeMember(t, pool, tc.root, tc.key, "in_progress", answered.LastSeq-1, record.P3, before)
				written, err := client.SetStatusAt(context.Background(), dispatch.Issue{Key: tc.key, LastSeq: answered.LastSeq - 1}, "in_progress")
				if err != nil {
					t.Fatalf("write the status of %s: %v", tc.key, err)
				}
				applyFact(t, pool, admission, "own-status-write", intake.OwnStatusWrite(written, "", ""))
				check(t, pool, "the status write")
			})
		})
	}
	t.Run("the updates of an issue above", func(t *testing.T) {
		pool := migratedPool(t)
		admission := shapeAdmission(t, 1)
		shapeRoot(t, pool, "ACME-7", record.P1)
		shapeMember(t, pool, "ACME-7", "ACME-13", "todo", 2, record.NoPriority, record.Resolved{Priority: record.P2, From: "OTHER-9", Known: true})
		for i, want := range []record.Resolved{{Priority: record.P0, From: "OTHER-5", Known: true}, {Known: true}} {
			raw := shapeEvents(t, "resolved-only.json")[i]
			apply(t, pool, admission, "above-"+string(rune('a'+i)), decodedShapeEvent(t, "ACME-13", raw), admission.engine)
			if got := issue(t, pool, "ACME-13"); got.Priority != record.NoPriority || got.Resolved != want {
				t.Fatalf("after update %d ACME-13 stores own %s and resolved %s, want own none and resolved %s", i, priorityName(got.Priority),
					resolvedName(got.Resolved), resolvedName(want))
			}
		}
	})
}

// With a Dispatch release that resolves no priority, the captured update of a child with no
// priority of its own, under a running tree whose root's own priority is P0, does not start it at a
// cap a holder below P0 fills: no Legion tree fallback makes it P0, and its record holds no
// resolved priority. The same update carrying an own P0 starts it outside the cap.
func TestAnOlderDispatchsPayloadPromotesNothing(t *testing.T) {
	update := string(shapeUpdate(t, "acme-1549-after-0.json"))
	for _, tc := range []struct {
		name   string
		own    string
		want   record.Priority
		starts bool
	}{
		{name: "no priority of its own", own: `"priority": null`, want: record.NoPriority},
		{name: "its own P0", own: `"priority": 0`, want: record.P0, starts: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(update, `"priority": null`, tc.own, 1)
			if !strings.Contains(raw, tc.own) {
				t.Fatalf("the captured update carries no %s", tc.own)
			}
			pool := migratedPool(t)
			admission := shapeAdmission(t, 1)
			seedSlottedInPhase(t, pool, shapeProject, "ACME-1", "A", phase.Admitted, func(issue *record.Issue) { issue.Priority = record.P1 })
			shapeRoot(t, pool, "ACME-1547", record.P0)
			shapeMember(t, pool, "ACME-1547", "ACME-1549", "todo", 1, record.NoPriority, record.Resolved{})

			apply(t, pool, admission, "update", decodedShapeEvent(t, "ACME-1549", []byte(raw)), admission.engine)
			tick(t, pool, admission, "tick")

			got := issue(t, pool, "ACME-1549")
			if got.LastDispatchSeq != 2 || got.Priority != tc.want || got.Resolved != (record.Resolved{}) {
				t.Fatalf("ACME-1549 after the captured update stands at sequence %d, own %s, resolved %s; want 2, own %s and an unknown resolved priority",
					got.LastDispatchSeq, priorityName(got.Priority), resolvedName(got.Resolved), priorityName(tc.want))
			}
			if holdsSlot(t, pool, "ACME-1549") != tc.starts {
				t.Fatalf("ACME-1549 holds a slot %t at a cap ACME-1 fills, want %t: only an issue's own P0 starts it where Dispatch resolves no priority",
					holdsSlot(t, pool, "ACME-1549"), tc.starts)
			}
		})
	}
}
