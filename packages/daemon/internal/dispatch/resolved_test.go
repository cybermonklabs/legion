package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The files under testdata/resolved are the bodies Dispatch serves for the three reads the daemon
// takes an issue's priority from (testdata/resolved/README). listing-acme-1549.json and
// detail-acme-1549.json are real: each is the answer of one GET, read on 2026-11-25 with the
// deployment's Dispatch token from a Dispatch release that sends `priority` and no
// `resolved_priority`, <P> being the project's key and <KEY> the issue's in that deployment:
//
//	curl -sS -H @<(printf 'Authorization: Bearer %s' "$(< "$DISPATCH_TOKEN_FILE")") "$DISPATCH_URL/api/v1/issues?project=<P>&all=true"
//	curl -sS -H @<(printf 'Authorization: Bearer %s' "$(< "$DISPATCH_TOKEN_FILE")") "$DISPATCH_URL/api/v1/issues/<KEY>"
//
// This repository is public, so each answer went through a filter before it was kept: the
// listing's through the first below, which keeps the issue's one row, and the detail's through the
// second. Both keep the issue's key, project, number, parent, status, rank, own priority, labels,
// sequence, repository, blockers and claim, with the two fields Dispatch's resolved-priority
// release adds, which this release does not send, and drop every other field, the title a person
// wrote among them.
//
//	jq '[.[] | select(.key == "<KEY>") | with_entries(select(.key | IN("key", "project", "number",
//	  "parent", "status", "rank", "priority", "resolved_priority", "priority_from", "labels",
//	  "last_seq", "resolved_repo", "blocked_by", "claim")))]'
//	jq 'with_entries(select(.key | IN("key", "project", "number", "parent", "status", "rank",
//	  "priority", "resolved_priority", "priority_from", "labels", "last_seq", "resolved_repo",
//	  "blocked_by", "claim")))'
//
// Then, in every string, the project's key became the neutral ACME, as AGENTS.md asks, so each
// issue keeps its number, and the repository the issue resolves to became acme/widgets; every
// other value is as the filter left it, and the files were then formatted with
// `bunx biome format --write`. patch-acme-1549.json is no capture: it is detail-acme-1549.json with
// the `advice` key a PATCH answer adds after the issue (patchIssue in
// packages/envoy/internal/dispatch/api/issue_patch.go answers withAdvice(written.issue,
// written.advice)). Every file named resolved-<v> is written by hand in the shape
// dispatch://INFRA-5793/spec#b-j-inherit and #b-j-events give, which the README quotes.

// resolvedBody is the body testdata/resolved/<name> holds, as Dispatch serves it.
func resolvedBody(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "resolved", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return body
}

// servedFields is what body, a listing's row or an issue Dispatch serves, says of the issue's own
// fields, read as plain JSON apart from the client, as the Issue the client gives for it: no
// resolved priority, which the test compares on its own, and no claim, which none of these bodies
// shows.
func servedFields(t *testing.T, body []byte) Issue {
	t.Helper()
	var served struct {
		Key          string   `json:"key"`
		Project      string   `json:"project"`
		Status       string   `json:"status"`
		Parent       *string  `json:"parent"`
		Rank         string   `json:"rank"`
		Priority     *int     `json:"priority"`
		Labels       []string `json:"labels"`
		LastSeq      int64    `json:"last_seq"`
		ResolvedRepo string   `json:"resolved_repo"`
		BlockedBy    []string `json:"blocked_by"`
		Claim        any      `json:"claim"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("read the issue's own fields of %s: %v", body, err)
	}
	if served.Claim != nil {
		t.Fatalf("%s shows a claim, which servedFields does not read", body)
	}
	return Issue{Key: served.Key, Project: served.Project, Status: served.Status, Parent: served.Parent, Rank: served.Rank, Priority: served.Priority,
		Labels: served.Labels, HandedOver: CarriesLegionLabel(served.Labels), LastSeq: served.LastSeq, ResolvedRepo: served.ResolvedRepo, BlockedBy: served.BlockedBy}
}

// The client reads `resolved_priority` and `priority_from` from each body Dispatch serves for the
// three reads the daemon takes an issue's priority from: a listing's row, the detail read and the
// issue a status write's PATCH answers. An older Dispatch's body, which carries no
// `resolved_priority`, reads as nil; a present null as a present none; a level as that level, with
// the issue it comes from. A level Dispatch never writes, 9, is kept as the read gave it
// (resolvedPriority), for intake to read as none (its dispatchResolved); a value that is no integer
// at all reads as a present none. Every row reads the issue's own fields as the body gives them.
func TestTheClientReadsResolvedPriorityFromEachBodyDispatchServes(t *testing.T) {
	level := func(n int) *int { return &n }
	for _, tc := range []struct {
		name string
		// file names the bodies a row serves, <read>-<file>.json for each read.
		file string
		// from and to change the body before it is served, for a value Dispatch writes none of.
		from, to string
		want     *ResolvedPriority
	}{
		{name: "the older shape, captured, which carries no resolved priority", file: "acme-1549"},
		{name: "resolved to P0 from an issue above", file: "resolved-0", want: &ResolvedPriority{Priority: level(0), From: "OTHER-5"}},
		{name: "resolved to P1 from an issue above", file: "resolved-1", want: &ResolvedPriority{Priority: level(1), From: "OTHER-5"}},
		{name: "resolved to P2 from an issue above", file: "resolved-2", want: &ResolvedPriority{Priority: level(2), From: "OTHER-5"}},
		{name: "resolved to P3 from an issue above", file: "resolved-3", want: &ResolvedPriority{Priority: level(3), From: "OTHER-5"}},
		{name: "resolved to null", file: "resolved-null", want: &ResolvedPriority{}},
		{name: "resolved to its own priority, from no issue", file: "resolved-own", want: &ResolvedPriority{Priority: level(2)}},
		{name: "a level Dispatch never writes", file: "resolved-0", from: `"resolved_priority": 0,`, to: `"resolved_priority": 9,`,
			want: &ResolvedPriority{Priority: level(9), From: "OTHER-5"}},
		{name: "no integer at all", file: "resolved-0", from: `"resolved_priority": 0,`, to: `"resolved_priority": "high",`,
			want: &ResolvedPriority{From: "OTHER-5"}},
	} {
		for _, read := range []string{"listing", "detail", "patch"} {
			t.Run(tc.name+"/"+read, func(t *testing.T) {
				body := resolvedBody(t, read+"-"+tc.file+".json")
				if tc.from != "" {
					changed := strings.Replace(string(body), tc.from, tc.to, 1)
					if changed == string(body) {
						t.Fatalf("%s-%s.json carries no %s to change", read, tc.file, tc.from)
					}
					body = []byte(changed)
				}
				issueBody := body
				if read == "listing" {
					var rows []json.RawMessage
					if err := json.Unmarshal(body, &rows); err != nil || len(rows) != 1 {
						t.Fatalf("the listing body holds %d rows (%v), want one", len(rows), err)
					}
					issueBody = rows[0]
				}
				served := servedFields(t, issueBody)
				path := "/api/v1/issues"
				if read != "listing" {
					path += "/" + served.Key
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assertBearer(t, r)
					method := http.MethodGet
					if read == "patch" {
						method = http.MethodPatch
					}
					if r.Method != method || r.URL.Path != path {
						t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.Path, method, path)
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(body)
				}))
				defer server.Close()
				client := New(server.URL, BearerTransport(dispatchToken, nil))

				var got Issue
				var err error
				switch read {
				case "listing":
					var issues []IssueSummary
					issues, err = client.ListIssues(context.Background(), "ACME", nil)
					if err == nil && len(issues) != 1 {
						t.Fatalf("the listing read %d issues, want the one row", len(issues))
					}
					if err == nil {
						got = issues[0].Issue()
					}
				case "detail":
					got, err = client.GetIssue(context.Background(), served.Key)
				case "patch":
					got, err = client.SetStatusAt(context.Background(), Issue{Key: served.Key, LastSeq: served.LastSeq - 1}, served.Status)
				}
				if err != nil {
					t.Fatalf("read the %s: %v", read, err)
				}
				if !reflect.DeepEqual(got.Resolved, tc.want) {
					t.Errorf("the %s's resolved priority = %s, want %s", read, describeResolved(got.Resolved), describeResolved(tc.want))
				}
				got.Resolved = nil
				if !reflect.DeepEqual(got, served) {
					t.Errorf("the %s's own fields = %+v, want %+v as the body gives them", read, got, served)
				}
			})
		}
	}
}

// describeResolved writes a read's resolved priority so that nil, a present none and a level read
// apart in a failure.
func describeResolved(resolved *ResolvedPriority) string {
	switch {
	case resolved == nil:
		return "nil (no resolved_priority)"
	case resolved.Priority == nil:
		return fmt.Sprintf("a present none from %q", resolved.From)
	default:
		return fmt.Sprintf("level %d from %q", *resolved.Priority, resolved.From)
	}
}
