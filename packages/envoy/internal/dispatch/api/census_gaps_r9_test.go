package api

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// censusGapsR9Read is one read a census-gap fix changed, as alice makes it, the item of the fix,
// and what its answer holds: the rows the changed statement reads for her.
type censusGapsR9Read struct {
	item, target string
	holds        []string
}

// TestCensusGapsWithNoPrivateProjectEveryChangedReadAnswersAsBefore (R9): with PRIV public again
// the deployment holds no private project, and alice, a full member, reads every route whose
// statement a census-gap fix bound to its reader with what that statement reads: an ask's
// followers (item 1), a comment's mention (item 2), an ask's backlink count (item 4), the issue's
// reference closure (item 8), its step runs (item 9), a going run's frames (item 10), and the
// project's Legion setting and board (item 11). Each answer is logged with its times and ids
// blanked, beside its sha256, for the report's comparison of the two commits; each must hold the
// rows the seed wrote, so a read that lost them fails here and does not pass by matching.
func TestCensusGapsWithNoPrivateProjectEveryChangedReadAnswersAsBefore(t *testing.T) {
	seed := seedPrivateProject(t)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	ctx := context.Background()
	issue := seed.issue[seedPublic]
	const peer = "session-r9-peer"

	seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+issue, map[string]any{"assignee": seedAlice}, http.StatusOK)
	document := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/projects/"+seedPublic+"/artifacts", map[string]any{
		"name": "r9-runbook.md", "content": "# Runbook\n\nShip it tonight.\n",
	}, http.StatusCreated)).Artifact
	ask := createdID(t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{
		"question": "Which runbook ships?", "options": []map[string]string{{"label": "Yes"}, {"label": "No"}},
	}, http.StatusCreated))
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/asks/"+ask+"/followers/"+peer, nil, http.StatusNoContent)
	comment := createdID(t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
		"body":     "See dispatch://" + issue + "/ask/" + ask + " and dispatch://" + seedPublic + "/artifact/" + document.Slug + " today.",
		"mentions": []map[string]any{{"target": "session:" + peer}},
	}, http.StatusCreated))
	seed.mustRequest(t, callerSharedToken, http.MethodPut, "/api/v1/issues/"+issue+"/steps/"+uuid.NewString(),
		implementerStart(daemonSessionOf(seedPublic)), http.StatusOK)
	going := createdID(t, seed.mustRequest(t, callerAlice, http.MethodPost, "/api/v1/going/requests",
		map[string]any{"words": "What does the runbook say?", "project": seedPublic}, http.StatusCreated))
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPublic+"/legion/daemon",
		map[string]any{"state_url": "http://legion-pub.internal:8080"}, http.StatusOK)
	if _, err := legion.RecordRead(ctx, seed.database.Pool, seedPublic, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), 42,
		[]legion.Observation{{IssueKey: issue, Field: legion.FieldPhase, Value: []byte(`"planning"`)}}); err != nil {
		t.Fatalf("record a read of PUB's daemon: %v", err)
	}

	follower := `"session_id":"` + peer + `"`
	for _, read := range []censusGapsR9Read{
		{"1 ask followers", "/api/v1/asks/" + ask + "/followers", []string{follower}},
		{"1 ask followers, 4 ask count", "/api/v1/asks/" + ask, []string{follower, `"referenced_by_count":1`}},
		{"1 Inbox followers", "/api/v1/inbox?assignee=" + seedAlice, []string{follower}},
		{"2 comment mentions", "/api/v1/comments/" + comment, []string{`"target":"session:` + peer + `"`}},
		{"2 issue comments' mentions", "/api/v1/issues/" + issue + "/comments", []string{`"target":"session:` + peer + `"`}},
		{"4 the issue's asks' counts", "/api/v1/issues/" + issue, []string{`"referenced_by_count":1`}},
		{"8 the reference closure", "/api/v1/issues/" + issue + "/references", []string{document.Slug}},
		{"9 the step runs", "/api/v1/issues/" + issue + "/steps", []string{`"role":"implementer"`}},
		{"10 the going stream", "/api/v1/going/requests/" + going + "/stream", []string{"event: "}},
		{"11 the Legion setting", "/api/v1/projects/" + seedPublic + "/legion/daemon", []string{"legion-pub.internal"}},
		{"11 the Legion board", "/api/v1/projects/" + seedPublic + "/legion/board", []string{issue}},
	} {
		body := seed.mustRequest(t, callerAlice, http.MethodGet, read.target, nil, http.StatusOK).Body.String()
		blanked := timestampsAndIDs.ReplaceAllString(body, "<varies>")
		t.Logf("R9 %s %s sha256 %x", read.item, timestampsAndIDs.ReplaceAllString(read.target, "<id>"), sha256.Sum256([]byte(blanked)))
		for _, held := range read.holds {
			if !strings.Contains(body, held) {
				t.Errorf("item %s: alice's GET %s holds no %s, as the base's did: %s", read.item, read.target, held, body)
			}
		}
	}
}
