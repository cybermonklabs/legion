package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The rows of the members of Dispatch's answers the load test decodes into plain strings and
// keeps or prints: whatever answers in Dispatch's place names a member that echoes the bearer the
// request carried (a GitHub-shaped token, written in two parts so no line of this file reads as a
// credential to a scanner) and quotes a URL with made-up credentials in it (redacttest.Code), and
// what the load test prints, or writes to a file, holds none of either. Each row asserts that
// first, so a row of a reverted site fails on an assertion that names the secret.

const memberBearer = "ghs_" + "0123456789abcdefghijklmnopqrstuvwxyz"

// memberUUID is an id in Dispatch's shape, for the members a row keeps clean.
const memberUUID = "0f8fad5b-d9cb-469f-a165-70867728950e"

// echoed is what a stand-in names in a member: the bearer r carried and the probe's URL.
func echoed(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + "-" + redacttest.Code
}

// errorText is err's text, "" for none.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func writeJSONAnswer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// TestASeedKeepsNoKeyOrIDThatEchoesItsRequest: Dispatch's issue create, or whatever answers in its
// place, names a key that echoes the seed's bearer and quotes a credential's URL. The seed refuses
// it, and neither its error (the key printed after a refused comment, or after a refused route)
// nor seed.json holds any of it, where it printed and kept the key as it came. A comment's id is
// kept only in Dispatch's shape too (seed-file). The controls: each error still names the issue,
// and an answer in Dispatch's shape is still kept as it came. Captures
// loadtest.answer-members/seed-<case>.
func TestASeedKeepsNoKeyOrIDThatEchoesItsRequest(t *testing.T) {
	stand := func(t *testing.T, key func(*http.Request) string, comment func(*http.Request) string) *api {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects":
				writeJSONAnswer(w, http.StatusCreated, map[string]string{"key": "LGA"})
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/issues":
				writeJSONAnswer(w, http.StatusCreated, map[string]string{"key": key(r), "primary_artifact_id": memberUUID})
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") && comment != nil:
				writeJSONAnswer(w, http.StatusCreated, map[string]string{"id": comment(r)})
			default:
				writeJSONAnswer(w, http.StatusBadRequest, map[string]string{"error": "refused", "code": "REFUSED"})
			}
		}))
		t.Cleanup(srv.Close)
		return newAPI(srv.URL, memberBearer, 4)
	}
	issueKey := func(*http.Request) string { return "LGA-1" }
	t.Run("comment", func(t *testing.T) {
		_, err := seedIssueWith(context.Background(), stand(t, echoed, nil), "ses-seed-0", "LGA", 0, 0, 1)
		redacttest.RequireNoPiece(t, "loadtest.answer-members/seed-comment", errorText(err), memberBearer)
		if !strings.Contains(errorText(err), "LGA-0") {
			t.Errorf("seedIssueWith = %v, want a refusal naming the issue's placeholder LGA-0", err)
		}
	})
	t.Run("route", func(t *testing.T) {
		_, err := seed(context.Background(), stand(t, echoed, nil), 0, 1)
		redacttest.RequireNoPiece(t, "loadtest.answer-members/seed-route", errorText(err), memberBearer)
		if !strings.Contains(errorText(err), "LGA-0") {
			t.Errorf("seed = %v, want a refusal naming the first issue's placeholder LGA-0", err)
		}
	})
	t.Run("seed-file", func(t *testing.T) {
		is, err := seedIssueWith(context.Background(), stand(t, issueKey, echoed), "ses-seed-0", "LGA", 0, 1, 1)
		file, _ := json.Marshal(seedFile{Issues: []seedIssue{is}})
		redacttest.RequireNoPiece(t, "loadtest.answer-members/seed-file", string(file)+" "+errorText(err), memberBearer)
		if !strings.Contains(errorText(err), "seed comment on LGA-1") {
			t.Errorf("seedIssueWith = %v, want a refusal naming the issue LGA-1", err)
		}
		kept, err := seedIssueWith(context.Background(), stand(t, issueKey, func(*http.Request) string { return memberUUID }), "ses-seed-0", "LGA", 0, 1, 1)
		if err != nil || kept.Key != "LGA-1" || kept.ArtifactID != memberUUID || len(kept.PlainCommentIDs) != 1 || kept.PlainCommentIDs[0] != memberUUID {
			t.Errorf("seedIssueWith over an answer in Dispatch's shape = %+v, %v; want its key and ids kept as they came", kept, err)
		}
	})
}

// TestAReferenceTargetKeepsNoArtifactIDThatEchoesItsRequest: Dispatch's answer to seed-refs'
// write of a target's first version, or whatever answers in its place, names an artifact id that
// echoes the bearer and quotes a credential's URL. seed-refs refuses it, where it sent the id back
// in the next version's read and printed it in that read's refusal. The control: the error still
// names the write. Captures loadtest.answer-members/ref-target.
func TestAReferenceTargetKeepsNoArtifactIDThatEchoesItsRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/issues/LGA-1/artifacts" {
			writeJSONAnswer(w, http.StatusCreated, map[string]any{"artifact": map[string]string{"id": echoed(r)}})
			return
		}
		writeJSONAnswer(w, http.StatusNotFound, map[string]string{"error": "no such document", "code": "NOT_FOUND"})
	}))
	t.Cleanup(srv.Close)
	_, err := writeRefTargets(context.Background(), newAPI(srv.URL, memberBearer, 2), refPlan{targets: []refTarget{{n: 1, issue: "LGA-1", versions: 2, order: []int{0, 1}}}})
	redacttest.RequireNoPiece(t, "loadtest.answer-members/ref-target", errorText(err), memberBearer)
	if !strings.Contains(errorText(err), "ref-target-1.md version 1 on LGA-1") {
		t.Errorf("writeRefTargets = %v, want a refusal naming the write of version 1", err)
	}
}

// TestAReplaySeedNamesNoCreatedKeyThatEchoesItsRequest: Dispatch's issue create, or whatever
// answers in its place, names a key that is not the bundle's, which echoes the seed's bearer and
// quotes a credential's URL. The seed's refusal names the key only in an issue key's shape, where
// it printed it as it came. The row hands mapIssue the key createIssue decodes, since createIssue
// first asks Postgres (*pgxpool.Pool) for an issue to adopt. The control: the refusal still names
// the issue's number and the key the bundle wants. Captures loadtest.answer-members/replay-key.
func TestAReplaySeedNamesNoCreatedKeyThatEchoesItsRequest(t *testing.T) {
	s := &replaySeeder{}
	err := s.mapIssue(context.Background(), seedIssuePlan{Key: "PRJ-1", Number: 1}, memberBearer+"-"+redacttest.Code, "")
	redacttest.RequireNoPiece(t, "loadtest.answer-members/replay-key", errorText(err), memberBearer)
	if !strings.Contains(errorText(err), "issue number 1 was created as") || !strings.Contains(errorText(err), "want PRJ-1") {
		t.Errorf("mapIssue = %v, want a refusal naming issue number 1 and the key PRJ-1", err)
	}
}
