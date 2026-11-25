package graphcensus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/seedcorpus"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/storesmove"
	"github.com/sjawhar/envoy/internal/dispatch/storesmove/storesmovetest"
)

// This file holds the writers TestEveryGraphKindHasOneWriter drives (one_writer_test.go), grouped by
// the package whose writers each group runs: the API's routes, the document service, the reference
// rebuild and the architecture importer over the rig's own database, and the backfill, the seed,
// the test corpus and the boot's rename pass over databases of their own.

// oneWriterState is what the cases leave for the cases after them.
type oneWriterState struct {
	issue, spec, notes, notesSlug, ask, comment, result string
	priority                                            string
	count                                               int
}

// oneWriterCheck is the check of the issue's Verification document the result cases record of.
const oneWriterCheck = "one-writer-check"

// oneWriterInstallation is the GitHub App installation the architecture source names.
const oneWriterInstallation = 23

// oneWriterCases are the writers the behavioural half drives, in the order they run.
func oneWriterCases() []writerCase {
	state := &oneWriterState{}
	next := func() int { state.count++; return state.count }
	return slices.Concat(apiCases(state, next), storesCases(state, next), documentCases(state, next), rigPackageCases(state, next), ownDatabaseCases())
}

// storesCases are the writers of the three kinds the move of dispatch://INFRA-1098's R32 took into
// the store, each through its route over the rig's issue: a priority's create, its rank move and
// its delete (prioritystore), an issue's advances (writeIssueAdvances), a run's start (PutStep), a
// plan (PutPlan) and a retro's rating. No table projects these kinds, so each case is store-only:
// its runs fail at the store write's commit, and must leave nothing.
func storesCases(state *oneWriterState, next func() int) []writerCase {
	daemon := sessionActor("legion-daemon:SIZE")
	createPriority := func(r *oneWriterRig) (string, error) {
		id := fmt.Sprintf("one-writer-goal-%d", next())
		_, err := r.send(http.MethodPost, "/api/v1/priorities", map[string]any{"id": id, "title": "Goal " + id}, "alice", http.StatusCreated)
		return id, err
	}
	created := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		id, err := createPriority(r)
		if err == nil && state.priority == "" {
			state.priority = id
		}
		return err
	}
	var anchor string
	anchored := func(t *testing.T, r *oneWriterRig, _ *store.Store) {
		var err error
		if anchor, err = createPriority(r); err != nil {
			t.Fatalf("create the priority a rank move lands beside: %v", err)
		}
	}
	// Each move lands the priority on the other side of the anchor, so every run moves it.
	moved := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		body, err := r.send(http.MethodGet, "/api/v1/priorities", nil, "alice", http.StatusOK)
		if err != nil {
			return err
		}
		side := "after"
		for _, row := range decodedOr[model.Priorities](body).Priorities {
			if row.ID == anchor {
				side = "before"
				break
			}
			if row.ID == state.priority {
				break
			}
		}
		_, err = r.send(http.MethodPatch, "/api/v1/priorities/"+state.priority, map[string]any{"rank": map[string]any{side: anchor}}, "alice", http.StatusOK)
		return err
	}
	var doomed string
	toDelete := func(t *testing.T, r *oneWriterRig, _ *store.Store) {
		var err error
		if doomed, err = createPriority(r); err != nil {
			t.Fatalf("create the priority a delete ends: %v", err)
		}
	}
	deleted := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		_, err := r.send(http.MethodDelete, "/api/v1/priorities/"+doomed, nil, "alice", http.StatusNoContent)
		return err
	}
	// Each write of the issue's advances states the set the issue does not hold, so every run
	// states or retracts a relation.
	advanced := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		body, err := r.send(http.MethodGet, "/api/v1/issues/"+state.issue, nil, "alice", http.StatusOK)
		if err != nil {
			return err
		}
		set := []string{state.priority}
		if len(decodedOr[model.Issue](body).Advances) > 0 {
			set = []string{}
		}
		_, err = r.send(http.MethodPatch, "/api/v1/issues/"+state.issue, map[string]any{"advances": set}, "alice", http.StatusOK)
		return err
	}
	started := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		_, err := r.send(http.MethodPut, "/api/v1/issues/"+state.issue+"/steps/"+uuid.NewString(), map[string]any{
			"actor": daemon, "task": "default", "step": 1, "kind": "step", "role": "planner", "generation": next(),
			"started_at": time.Now().UTC().Format(time.RFC3339),
		}, "", http.StatusOK)
		return err
	}
	planned := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		_, err := r.send(http.MethodPut, "/api/v1/issues/"+state.issue+"/plans/"+uuid.NewString(), map[string]any{
			"actor": daemon, "generation": next(), "source": "default",
			"tasks": []map[string]any{{"id": "default", "steps": []string{"planner", "implementer"}}},
		}, "", http.StatusOK)
		return err
	}
	var retro string
	var retroGeneration int
	retroIdentity := func() map[string]any {
		return map[string]any{"actor": daemon, "task": "default", "step": 0, "kind": "retro", "role": "implementer", "generation": retroGeneration}
	}
	retroEnded := func(t *testing.T, r *oneWriterRig, _ *store.Store) {
		retro, retroGeneration = uuid.NewString(), next()
		body := retroIdentity()
		body["result"], body["ended_at"] = "passed", time.Now().UTC().Format(time.RFC3339)
		if _, err := r.send(http.MethodPut, "/api/v1/issues/"+state.issue+"/steps/"+retro, body, "", http.StatusOK); err != nil {
			t.Fatalf("end the retro a rating rates: %v", err)
		}
	}
	rated := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		body := retroIdentity()
		body["rating"], body["rating_reason"] = 4, "It held."
		_, err := r.send(http.MethodPut, "/api/v1/issues/"+state.issue+"/steps/"+retro+"/rating", body, "", http.StatusOK)
		return err
	}
	return []writerCase{
		{name: "a priority's create", storeOnly: true, drive: created},
		{name: "a priority's rank move", storeOnly: true, ready: anchored, drive: moved},
		{name: "a priority's delete", storeOnly: true, each: toDelete, drive: deleted},
		{name: "an issue's advances", storeOnly: true, drive: advanced},
		{name: "a run's start", storeOnly: true, drive: started},
		{name: "a plan", storeOnly: true, drive: planned},
		{name: "a retro's rating", storeOnly: true, each: retroEnded, drive: rated},
	}
}

// createIssue creates an issue of the rig's project as alice and answers its key and its spec.
func createIssue(r *oneWriterRig, title string) (string, string, error) {
	body, err := r.send(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "SIZE", "title": title, "spec": "# Spec\n\nWhat one writer writes.\n", "force": true,
	}, "alice", http.StatusCreated)
	if err != nil {
		return "", "", err
	}
	created := decodedOr[struct {
		Key     string `json:"key"`
		Primary string `json:"primary_artifact_id"`
	}](body)
	return created.Key, created.Primary, nil
}

// upload uploads, as alice, a document of the rig's issue and answers its id and its slug.
func upload(r *oneWriterRig, issue, name, content string) (string, string, error) {
	body, err := r.send(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]any{"name": name, "content": content}, "alice", http.StatusCreated)
	if err != nil {
		return "", "", err
	}
	artifact := decodedOr[struct {
		Artifact model.Artifact `json:"artifact"`
	}](body).Artifact
	return artifact.ID, artifact.Slug, nil
}

// apiCases are the writers the API's routes run.
func apiCases(state *oneWriterState, next func() int) []writerCase {
	createdIssue := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		key, spec, err := createIssue(r, fmt.Sprintf("A writer's issue %d", next()))
		if err == nil && state.issue == "" {
			state.issue, state.spec, r.issue, r.spec = key, spec, key, spec
		}
		return err
	}
	uploadedNotes := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		id, slug, err := upload(r, state.issue, fmt.Sprintf("notes-%d.md", next()), "# Notes\n\nA paragraph citing dispatch://"+state.issue+".\n")
		if err == nil && state.notes == "" {
			state.notes, state.notesSlug = id, slug
		}
		return err
	}
	edited := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		_, err := r.send(http.MethodPost, "/api/v1/artifacts/"+state.notes+"/edits", map[string]any{
			"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": fmt.Sprintf("A paragraph edit %d adds.", next())}},
		}, "alice", http.StatusOK)
		return err
	}
	commented := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		body, err := r.send(http.MethodPost, "/api/v1/issues/"+state.issue+"/comments",
			map[string]any{"body": fmt.Sprintf("A remark %d on the issue.", next())}, "alice", http.StatusCreated)
		if err == nil && state.comment == "" {
			state.comment = decodedOr[model.Comment](body).ID
		}
		return err
	}
	anchored := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		_, err := r.send(http.MethodPost, "/api/v1/issues/"+state.issue+"/comments", map[string]any{
			"body": fmt.Sprintf("A remark %d on the passage.", next()), "anchor": map[string]any{"artifact": state.notesSlug, "quote": "A paragraph citing"},
		}, "alice", http.StatusCreated)
		return err
	}
	personAsked := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		_, err := r.send(http.MethodPost, "/api/v1/issues/"+state.issue+"/asks",
			map[string]any{"question": fmt.Sprintf("Which writer writes %d?", next())}, "alice", http.StatusCreated)
		return err
	}
	sessionAsked := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		body, err := r.send(http.MethodPost, "/api/v1/issues/"+state.issue+"/asks", map[string]any{
			"question": fmt.Sprintf("Which writer writes %d?", next()), "actor": sessionActor("ses_asker"),
		}, "", http.StatusCreated)
		if err == nil && state.ask == "" {
			state.ask = decodedOr[model.Ask](body).ID
		}
		return err
	}
	followed := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		session := fmt.Sprintf("ses_follower_%d", next())
		_, err := r.send(http.MethodPut, "/api/v1/asks/"+state.ask+"/followers/"+session, map[string]any{"actor": sessionActor(session)}, "", http.StatusNoContent)
		return err
	}
	messaged := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		_, err := r.send(http.MethodPost, "/api/v1/issues/"+state.issue+"/messages",
			map[string]any{"body": fmt.Sprintf("A note %d on the issue.", next())}, "alice", http.StatusCreated)
		return err
	}
	asked := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		body, err := r.send(http.MethodPost, "/api/v1/going/requests", map[string]any{"words": fmt.Sprintf("What does writer %d write?", next()), "project": "SIZE"}, "alice", http.StatusCreated)
		if err != nil {
			return err
		}
		run := decodedOr[struct {
			ID string `json:"id"`
		}](body).ID
		return r.waitFor(fmt.Sprintf(`select exists (select 1 from going_events where request_id = '%s'::uuid)`, run), 30*time.Second)
	}
	var child, replied, answered, unfollowed, target, filedFrom string
	parented := func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		_, err := r.send(http.MethodPatch, "/api/v1/issues/"+child, map[string]any{"parent": state.issue}, "alice", http.StatusOK)
		return err
	}
	return []writerCase{{
		name: "the issue create", tables: []string{"issues", "artifacts", "artifact_versions", "document_blocks", "issue_room_blocks"},
		drive: createdIssue, again: createdIssue,
	}, {
		name: "an issue document's upload", tables: []string{"artifacts", "artifact_versions", "document_blocks", "block_refs", "refs"},
		drive: uploadedNotes, again: uploadedNotes,
	}, {
		name: "a document's edit", tables: []string{"artifact_versions", "document_blocks", "document_block_texts"},
		drive: edited, again: edited,
	}, {
		name: "a comment on the issue", tables: []string{"comments", "user_threads"},
		drive: commented, again: commented,
	}, {
		name: "a comment anchored to a document", tables: []string{"comments"},
		drive: anchored, again: anchored,
	}, {
		name: "a session's reply to a mention", tables: []string{"comments"},
		ready: func(t *testing.T, r *oneWriterRig, _ *store.Store) {
			replied = decoded[model.Comment](t, r.request(t, http.MethodPost, "/api/v1/issues/"+state.issue+"/comments", map[string]any{
				"body": "Can you take this, @" + refusedSession + "?", "mentions": []map[string]any{{"target": "session:" + refusedSession}},
			}, "alice", http.StatusCreated)).ID
			r.settledAttempts(t, replied, "failed")
		},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodPost, "/api/v1/comments/"+replied+"/reply", map[string]any{
				"actor": sessionActor(refusedSession), "attempt": 1, "body": "Yes, I take it.",
			}, "", http.StatusCreated)
			return err
		},
	}, {
		name: "a person's ask on the issue", tables: []string{"asks", "user_threads"},
		drive: personAsked, again: personAsked,
	}, {
		name: "a session's ask on the issue", tables: []string{"asks", "ask_followers"},
		drive: sessionAsked, again: sessionAsked,
	}, {
		name: "a session's follow of an ask", tables: []string{"ask_followers"},
		drive: followed, again: followed,
	}, {
		name: "a session's unfollow of an ask", tables: []string{"ask_followers"},
		ready: func(t *testing.T, r *oneWriterRig, _ *store.Store) {
			unfollowed = fmt.Sprintf("ses_unfollower_%d", next())
			r.request(t, http.MethodPut, "/api/v1/asks/"+state.ask+"/followers/"+unfollowed, map[string]any{"actor": sessionActor(unfollowed)}, "", http.StatusNoContent)
		},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodDelete, "/api/v1/asks/"+state.ask+"/followers/"+unfollowed, map[string]any{"actor": sessionActor(unfollowed)}, "", http.StatusNoContent)
			return err
		},
	}, {
		name: "a person's answer to an ask", tables: []string{"asks", "user_threads"},
		ready: func(t *testing.T, r *oneWriterRig, _ *store.Store) {
			answered = decoded[model.Ask](t, r.request(t, http.MethodPost, "/api/v1/issues/"+state.issue+"/asks", map[string]any{
				"question": "Which writer answers?", "actor": sessionActor("ses_asker"),
			}, "", http.StatusCreated)).ID
		},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodPost, "/api/v1/asks/"+answered+"/answer", map[string]any{"text": "The one writer.", "expected_edited_at": nil}, "alice", http.StatusOK)
			return err
		},
	}, {
		name: "a message on the issue", tables: []string{"user_threads"},
		drive: messaged, again: messaged,
	}, {
		name: "an approval request of the spec", tables: []string{"asks"},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodPost, "/api/v1/artifacts/"+state.spec+"/approval-requests", map[string]any{"actor": sessionActor("ses_requester")}, "", http.StatusCreated)
			return err
		},
	}, {
		name: "a run of the command bar", tables: []string{"going_requests", "going_events"},
		drive: asked, again: asked,
	}, {
		name: "an issue's parent", tables: []string{"issues"},
		ready: func(t *testing.T, r *oneWriterRig, _ *store.Store) {
			key, _, err := createIssue(r, "A child the parent write names")
			if err != nil {
				t.Fatalf("create the child: %v", err)
			}
			child = key
		},
		drive: parented,
		again: func(t *testing.T, r *oneWriterRig, database *store.Store) error {
			key, _, err := createIssue(r, fmt.Sprintf("A child %d", next()))
			if err != nil {
				return err
			}
			_, err = r.send(http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"parent": state.issue}, "alice", http.StatusOK)
			return err
		},
	}, {
		name: "an issue's needs and its filed_from at a comment the release before wrote", tables: []string{"issue_links", "comments"},
		ready: func(t *testing.T, r *oneWriterRig, database *store.Store) {
			key, _, err := createIssue(r, "What the issue waits on")
			if err != nil {
				t.Fatalf("create the issue waited on: %v", err)
			}
			target = key
			if err := database.Pool.QueryRow(context.Background(), `
				insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', 'Words the release before wrote.')
				returning id::text`, state.issue).Scan(&filedFrom); err != nil {
				t.Fatalf("write the comment the issue is filed from as the release before wrote it: %v", err)
			}
		},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodPatch, "/api/v1/issues/"+state.issue, map[string]any{
				"needs": []string{target}, "filed_from": "dispatch://" + state.issue + "/comment/" + filedFrom,
			}, "alice", http.StatusOK)
			return err
		},
		again: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			waiting, _, err := createIssue(r, fmt.Sprintf("An issue that waits %d", next()))
			if err != nil {
				return err
			}
			waited, _, err := createIssue(r, fmt.Sprintf("An issue waited on %d", next()))
			if err != nil {
				return err
			}
			_, err = r.send(http.MethodPatch, "/api/v1/issues/"+waiting, map[string]any{"needs": []string{waited}}, "alice", http.StatusOK)
			return err
		},
	}, {
		name: "a result's record", tables: []string{"check_results"},
		ready: func(t *testing.T, r *oneWriterRig, _ *store.Store) {
			r.request(t, http.MethodPost, "/api/v1/issues/"+state.issue+"/artifacts", map[string]any{
				"name": "verification.md", "content": ":::check{#" + oneWriterCheck + " surface=\"cli\"}\nThe writer writes once.\n:::\n",
			}, "alice", http.StatusCreated)
		},
		drive: recorded(state), again: recorded(state),
	}, {
		name: "a result's retraction", tables: []string{"check_result_retractions"},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodPost, "/api/v1/results/"+state.result+"/retract", map[string]any{"reason": "The run measured the wrong build."}, "alice", http.StatusCreated)
			return err
		},
		again: func(t *testing.T, r *oneWriterRig, database *store.Store) error {
			result, err := recordResult(r, state.issue)
			if err != nil {
				return err
			}
			_, err = r.send(http.MethodPost, "/api/v1/results/"+result+"/retract", map[string]any{"reason": "Another run measured the wrong build."}, "alice", http.StatusCreated)
			return err
		},
	}, {
		name: "a file's upload", tables: []string{"artifacts", "artifact_versions"},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			return uploadFile(r, state.issue, fmt.Sprintf("data-%d.bin", next()), []byte("bytes no reader renders\x00\x01"))
		},
	}, {
		name: "the inbox's read mark", tables: []string{"user_threads"}, each: unreadThread(state),
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodPost, "/api/v1/me/inbox/read", map[string]any{"read_through": markedThrough()}, "alice", 0)
			return err
		},
	}, {
		name: "a thread's read mark", tables: []string{"user_threads"}, each: unreadThread(state),
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodPut, "/api/v1/me/threads/"+state.comment+"/read", map[string]any{"read_through": markedThrough()}, "alice", 0)
			return err
		},
	}, {
		name: "the inbox's dismissal", tables: []string{"user_threads"}, each: unreadThread(state),
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodPost, "/api/v1/me/inbox/dismiss", map[string]any{"dismiss_through": markedThrough()}, "alice", 0)
			return err
		},
	}}
}

// recordResult records, as alice, a failed result of oneWriterCheck on issue and answers its id.
func recordResult(r *oneWriterRig, issue string) (string, error) {
	body, err := r.send(http.MethodPost, "/api/v1/issues/"+issue+"/results", map[string]any{
		"check": oneWriterCheck, "role": "implementer", "verdict": "fail", "commit": "1111111111111111111111111111111111111111",
		"negative_control": "Broke what the check proves and the run failed.", "failed": 1,
	}, "alice", http.StatusCreated)
	if err != nil {
		return "", err
	}
	return decodedOr[struct {
		Result model.CheckResult `json:"result"`
	}](body).Result.ID, nil
}

// recorded is the drive of a result's record, which leaves the result for the retraction.
func recorded(state *oneWriterState) func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
	return func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		result, err := recordResult(r, state.issue)
		if err == nil && state.result == "" {
			state.result = result
		}
		return err
	}
}

// unreadThread gives alice's thread of state.comment a reply she has not read, from a session, so
// each run of a read mark or a dismissal has a thread to write over.
func unreadThread(state *oneWriterState) func(t *testing.T, r *oneWriterRig, _ *store.Store) {
	return func(t *testing.T, r *oneWriterRig, _ *store.Store) {
		r.request(t, http.MethodPost, "/api/v1/issues/"+state.issue+"/comments", map[string]any{
			"body": "A reply alice has not read.", "reply_to": state.comment, "actor": sessionActor("ses_replier"),
		}, "", http.StatusCreated)
	}
}

// markedThrough is the moment a read mark or a dismissal is written through: half a minute ahead,
// which the server stores as its own now, so the mark covers the reply unreadThread wrote within
// the same second, which a moment cut to the second would leave unread.
func markedThrough() string {
	return time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339)
}

// uploadFile uploads name with content as a file of issue, as a browser's multipart upload does.
func uploadFile(r *oneWriterRig, issue, name string, content []byte) error {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("name", name); err != nil {
		return err
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, name))
	header.Set("Content-Type", "application/octet-stream")
	part, err := form.CreatePart(header)
	if err != nil {
		return err
	}
	if _, err := part.Write(content); err != nil {
		return err
	}
	if err := form.Close(); err != nil {
		return err
	}
	status, answered := r.serve(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", body.Bytes(), "alice", form.FormDataContentType())
	if status != http.StatusCreated {
		return fmt.Errorf("upload %s: status=%d body=%s", name, status, firstLineOf(string(answered)))
	}
	return nil
}

// documentCases are the writers the document service runs: an edit's move and deletion of a block,
// a suggestion's anchor orphaned and refreshed, the anchor block backfill, an ask block's
// settlement, the block-index tick, a room's settlement and a document's removal.
func documentCases(state *oneWriterState, next func() int) []writerCase {
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	var placed, deleted, orphaned, refreshed, pinned, asked, ticked, roomIssue, roomSpec, removable string
	var refreshedBase map[string]string
	const holderAndMover = "::::callout{#holder kind=\"note\" title=\"\"}\nThe holder.\n\n:::callout{#mover kind=\"note\" title=\"\"}\nThe mover.\n:::\n\n::::\n"
	const citing = "A paragraph citing dispatch://%s to delete."
	return []writerCase{{
		name: "an edit that moves a block out of the block holding it", tables: []string{"document_blocks"},
		ready: func(t *testing.T, r *oneWriterRig, _ *store.Store) {
			placed = r.uploaded(t, "placement.md", holderAndMover)
		},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodPost, "/api/v1/artifacts/"+placed+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "move", "block": "mover", "after": "block:holder"}},
			}, "alice", http.StatusOK)
			return err
		},
	}, {
		name: "an edit that deletes a paragraph holding a reference", tables: []string{"document_blocks", "block_refs"},
		ready: func(t *testing.T, r *oneWriterRig, _ *store.Store) {
			deleted = r.uploaded(t, "deleted.md", "# Deleted\n\nA paragraph that stays.\n\n"+fmt.Sprintf(citing, state.issue)+"\n")
		},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodPost, "/api/v1/artifacts/"+deleted+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "delete", "find": fmt.Sprintf(citing, state.issue)}},
			}, "alice", http.StatusOK)
			return err
		},
	}, {
		name: "an accept that finds a suggestion's mark gone", tables: []string{"comments"},
		ready: func(t *testing.T, r *oneWriterRig, database *store.Store) {
			const words = "Words the suggestion orphans"
			document := r.uploaded(t, "suggested.md", "# Suggested\n\n"+words+".\n")
			comment := r.suggestion(t, document, words)
			orphaned = comment.ID
			block := r.anchoredBlock(t, document, *comment.Anchor.BlockID)
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+document+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "delete", "find": words + "."}},
			}, "alice", http.StatusOK)
			if _, err := database.Pool.Exec(ctx, `update comments set anchor = jsonb_set(anchor, '{orphaned}', 'false') where id = $1::uuid`, orphaned); err != nil {
				t.Fatalf("stand the suggestion's anchor up again: %v", err)
			}
			r.writeAnchoredIn(t, orphaned, block, true)
		},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodPost, "/api/v1/comments/"+orphaned+"/accept", map[string]any{}, "alice", http.StatusConflict)
			return err
		},
	}, {
		name: "an upload that takes a suggestion's quote out", tables: []string{"comments", "artifact_versions"},
		drives: []string{"internal/dispatch/docs/anchor_refresh.go#refreshAnchors"},
		ready: func(t *testing.T, r *oneWriterRig, _ *store.Store) {
			document := r.uploaded(t, "refreshed.md", "# Refreshed\n\nThe moon is up.\n")
			refreshed = r.suggestion(t, document, "moon is up").ID
			refreshedBase = r.uploadBase(t, document)
		},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodPost, "/api/v1/issues/"+state.issue+"/artifacts", map[string]any{
				"name": "refreshed.md", "content": "# Refreshed\n\nStars come out.\n", "precondition": refreshedBase,
			}, "alice", http.StatusCreated)
			return err
		},
		settle: func(t *testing.T, r *oneWriterRig, _ *store.Store, within time.Duration) bool {
			return r.waitFor(fmt.Sprintf(`select coalesce((anchor->>'orphaned')::boolean, false) from comments where id = '%s'::uuid`, refreshed), within) == nil
		},
	}, {
		name: "the anchor block backfill", tables: []string{"comments"}, batches: true,
		ready: func(t *testing.T, r *oneWriterRig, database *store.Store) {
			document := r.uploaded(t, "pinned.md", "# Pinned\n\nA lazy dog sleeps.\n")
			comment := decoded[model.Comment](t, r.request(t, http.MethodPost, "/api/v1/issues/"+state.issue+"/comments", map[string]any{
				"body": "A remark on the dog.", "anchor": map[string]any{"artifact": document, "quote": "lazy dog"},
			}, "alice", http.StatusCreated))
			pinned = comment.ID
			block := r.anchoredBlock(t, document, *comment.Anchor.BlockID)
			if _, err := database.Pool.Exec(ctx, `update comments set anchor = anchor - 'block_id' where id = $1::uuid`, pinned); err != nil {
				t.Fatalf("write the comment's anchor as the release before wrote it, with no block: %v", err)
			}
			r.writeAnchoredIn(t, pinned, block, false)
			ask := decoded[model.Ask](t, r.request(t, http.MethodPost, "/api/v1/issues/"+state.issue+"/asks", map[string]any{
				"question": "Does the dog sleep?", "anchor": map[string]any{"artifact": document, "quote": "dog sleeps"},
			}, "alice", http.StatusCreated))
			if _, err := database.Pool.Exec(ctx, `update asks set anchor = anchor - 'block_id' where id = $1::uuid`, ask.ID); err != nil {
				t.Fatalf("write the ask's anchor as the release before wrote it, with no block: %v", err)
			}
		},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			backfilled, err := r.documents.BackfillAnchorBlocks(ctx)
			if err != nil {
				return err
			}
			if backfilled.Comments == 0 {
				return fmt.Errorf("the anchor block backfill pinned %+v, want the comment %s", backfilled, pinned)
			}
			return nil
		},
	}, {
		name: "a settlement that opens an ask a session wrote as a block", tables: []string{"asks"},
		ready: func(t *testing.T, r *oneWriterRig, _ *store.Store) {
			asked = r.uploaded(t, "asking.md", "# Asking\n\nA paragraph before the ask.\n")
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+asked+"/edits", map[string]any{
				"actor": sessionActor("ses_editor"),
				"ops":   []map[string]any{{"op": "insert", "after": "end", "markdown": ":::ask{#one-writer-ask}\nWhich writer opens this?\n:::"}},
			}, "", http.StatusOK)
		},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			r.documents.SettleNow(asked)
			return nil
		},
		settle: func(t *testing.T, r *oneWriterRig, _ *store.Store, within time.Duration) bool {
			return r.waitFor(fmt.Sprintf(`select exists (select 1 from asks where block_artifact_id = '%s'::uuid and block_id = 'one-writer-ask')`, asked), within) == nil
		},
	}, {
		name: "the block-index tick over a version a build without the block index wrote", tables: []string{"document_blocks", "block_refs"},
		ready: func(t *testing.T, r *oneWriterRig, database *store.Store) {
			ticked = r.uploaded(t, "ticked.md", "# Ticked\n\nA paragraph the tick indexes.\n")
			if _, err := r.documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
				t.Fatalf("run the block-index tick over what the cases before left owed: %v", err)
			}
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			joined, ledger := r.documents.Join(ctx, tx)
			defer ledger.Discard()
			// The paragraph cites the issue, so the document holds a block mention, a block_refs row.
			cited := "A paragraph a live edit adds, citing dispatch://" + state.issue + "."
			if edit, err := r.documents.ApplyOps(joined, ticked, []model.EditOp{{Op: "insert", After: "end", Markdown: cited}}, alice, nil); err != nil || !edit.Changed {
				t.Fatalf("edit %s live (changed %v): %v", ticked, edit.Changed, err)
			}
			if err := ledger.Commit(ctx); err != nil {
				t.Fatalf("commit the live edit: %v", err)
			}
			live, _, err := r.documents.TextWithBlocks(ctx, ticked)
			if err != nil {
				t.Fatalf("read the live text: %v", err)
			}
			if _, err := database.Pool.Exec(ctx, `
				insert into artifact_versions (artifact_id, number, markdown, authors)
				select $1::uuid, max(number) + 1, $2, '[{"kind":"user","id":"alice"}]' from artifact_versions where artifact_id = $1::uuid
			`, ticked, live); err != nil {
				t.Fatalf("write the live edit as a version a build without the block index writes: %v", err)
			}
			// The mention's row names no node, as a build before the store wrote it: the tick over
			// the stale version names it (refs' queueBlockRefs, the naming of surviving rows).
			if _, err := database.Pool.Exec(ctx, `update block_refs set node_kind = null, node_address = null where artifact_id = $1::uuid`, ticked); err != nil {
				t.Fatalf("write the mention's row as a build before the store wrote it: %v", err)
			}
		},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			tick, err := r.documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil)
			if err != nil {
				return err
			}
			if tick.Documents == 0 {
				return errors.New("the tick indexed no document")
			}
			return nil
		},
	}, {
		name: "a settlement of an issue's room", tables: []string{"artifact_versions", "document_blocks"},
		ready: func(t *testing.T, r *oneWriterRig, database *store.Store) {
			key, spec, err := createIssue(r, "What the room settles")
			if err != nil {
				t.Fatalf("create the room's issue: %v", err)
			}
			roomIssue, roomSpec = key, spec
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			joined, ledger := r.documents.Join(ctx, tx)
			defer ledger.Discard()
			if edit, err := r.documents.ApplyOps(joined, roomSpec, []model.EditOp{{Op: "insert", After: "end", Markdown: "A paragraph the room settles."}}, alice, nil); err != nil || !edit.Changed {
				t.Fatalf("edit the spec of %s live (changed %v): %v", roomIssue, edit.Changed, err)
			}
			if err := ledger.Commit(ctx); err != nil {
				t.Fatalf("commit the live edit: %v", err)
			}
		},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			return r.documents.SettleIssueDocuments(ctx, roomIssue)
		},
		settle: func(t *testing.T, r *oneWriterRig, _ *store.Store, within time.Duration) bool {
			return r.waitFor(fmt.Sprintf(`select exists (select 1 from artifact_versions where artifact_id = '%s'::uuid and number = 2)`, roomSpec), within) == nil
		},
	}, {
		name: "a document's removal", tables: []string{"artifacts"},
		ready: func(t *testing.T, r *oneWriterRig, _ *store.Store) {
			removable = decoded[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+state.issue+"/artifacts", map[string]any{"name": fmt.Sprintf("scratch-%d.md", next())}, "alice", http.StatusCreated)).Artifact.ID
		},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodDelete, "/api/v1/artifacts/"+removable, nil, "alice", http.StatusNoContent)
			return err
		},
	}}
}

// rigPackageCases are the writers of the reference rebuild and the architecture importer over the
// rig's own database.
func rigPackageCases(state *oneWriterState, next func() int) []writerCase {
	ctx := context.Background()
	var rebuilt string
	var importer *architecture.Importer
	return []writerCase{{
		name: "the rebuild of every reference", tables: []string{"refs"},
		ready: func(t *testing.T, r *oneWriterRig, database *store.Store) {
			rebuilt = r.uploaded(t, "rebuilt.md", "# Rebuilt\n\nCites dispatch://"+state.issue+".\n")
			// The references a build that held none wrote: the rows of this document gone, which the
			// rebuild writes again.
			if _, err := database.Pool.Exec(ctx, `delete from refs where from_kind = 'artifact' and from_id = $1`, rebuilt); err != nil {
				t.Fatalf("take the references of rebuilt.md out as a build that held none left them: %v", err)
			}
		},
		drive: func(t *testing.T, r *oneWriterRig, database *store.Store) error {
			done, err := refs.RebuildAll(ctx, database.Pool, writersServerURL)
			if err == nil && done.Documents == 0 {
				err = fmt.Errorf("the rebuild reports %+v, want rebuilt.md's references rebuilt", done)
			}
			return err
		},
	}, {
		name: "the architecture import", tables: []string{"components", "component_depends"},
		ready: func(t *testing.T, r *oneWriterRig, database *store.Store) {
			if _, err := database.Pool.Exec(ctx, `
				insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
				values ('SIZE', $1, 'main', $2, '{"kind":"user","id":"alice"}')`, graphcensus.OneWriterArchitectureRepo, oneWriterInstallation); err != nil {
				t.Fatalf("write the project's architecture source: %v", err)
			}
			// The importer outlives the case, as the two writers at once run it again.
			importer = graphcensus.OneWriterImporter(r.top, database, oneWriterInstallation)
		},
		drive: imported(&importer), again: imported(&importer),
	}, {
		name: "an issue's components", tables: []string{"issue_component_members"},
		drive: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			_, err := r.send(http.MethodPatch, "/api/v1/issues/"+state.issue, map[string]any{
				"components": map[string]any{"mode": "explicit", "ids": []string{"api"}},
			}, "alice", http.StatusOK)
			return err
		},
		again: func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
			key, _, err := createIssue(r, fmt.Sprintf("Another issue of the api component %d", next()))
			if err != nil {
				return err
			}
			_, err = r.send(http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
				"components": map[string]any{"mode": "explicit", "ids": []string{"api"}},
			}, "alice", http.StatusOK)
			return err
		},
	}}
}

// imported is the drive of the architecture import of the project's one source, through the
// importer its case's ready builds.
func imported(importer **architecture.Importer) func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
	return func(t *testing.T, r *oneWriterRig, _ *store.Store) error {
		_, err := (*importer).Sync(context.Background(), "SIZE", graphcensus.OneWriterArchitectureRepo)
		return err
	}
}

// oneWriterRepositoryRoot is the checkout's root, five directories above this package.
func oneWriterRepositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join(oneWriterModule, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// releaseBeforeRows are the rows a build before the store wrote, each naming no node, which the
// backfill gives a node: an issue's document, its version and one of its blocks with the link the
// block holds, a comment, an ask and its follower, a wait, and a project's components, the
// dependency between two of them and the issue's component.
var releaseBeforeRows = []string{
	`insert into projects (key, name, default_assignee) values ('OLD', 'Old', 'alice')`,
	`insert into issues (key, project_key, number, title, created_by, rank) values
		('OLD-1', 'OLD', 1, 'One', '{"kind":"user","id":"alice"}', 'U'),
		('OLD-2', 'OLD', 2, 'Two', '{"kind":"user","id":"alice"}', 'V')`,
	`insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('00000000-0000-4000-8000-00000000d001', 'OLD-1', 'OLD', 'spec', 'spec.md', 'doc', false, '{"kind":"user","id":"alice"}')`,
	`insert into artifact_versions (artifact_id, number, markdown, authors)
		values ('00000000-0000-4000-8000-00000000d001', 1, 'Written before the store existed, citing dispatch://OLD-2.', '[{"kind":"user","id":"alice"}]')`,
	`insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		select id, 'only', ref_key || '#only', 'paragraph', '', 'Block only.', 1, now() from artifacts where id = '00000000-0000-4000-8000-00000000d001'`,
	`insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
		select id, 'only', ref_key || '#only', 'issue', 'OLD-2', 'mentions', 1 from artifacts where id = '00000000-0000-4000-8000-00000000d001'`,
	`insert into comments (issue_key, author, body) values ('OLD-1', '{"kind":"user","id":"alice"}', 'Words the release before wrote.')`,
	`insert into asks (id, issue_key, author, question) values ('00000000-0000-4000-8000-00000000a001', 'OLD-1', '{"kind":"session","id":"ses_asked"}', 'Which store?')`,
	`insert into ask_followers (ask_id, session_id) values ('00000000-0000-4000-8000-00000000a001', 'ses_follows')`,
	`insert into issue_links (issue_key, kind, target_key, created_at) values ('OLD-1', 'blocked_by', 'OLD-2', now() - interval '3 days')`,
	`insert into architecture_sources (id, project_key, repo, branch, installation_id, created_by)
		values (9001, 'OLD', 'old/arch', 'main', 1, '{"kind":"user","id":"alice"}')`,
	`insert into architecture_snapshots (id, project_key, commit, imported_at, files) values (9001, 'OLD', 'one', now() - interval '2 days', '{}')`,
	`insert into components (project_key, id, title, prose, parent, snapshot_id, source_id) values
		('OLD', 'store', 'Store', 'The store.', null, 9001, 9001), ('OLD', 'census', 'Census', 'The census.', 'store', 9001, 9001)`,
	`insert into component_depends (project_key, from_id, to_id) values ('OLD', 'census', 'store')`,
	`insert into issue_components (issue_key, mode, updated_at) values ('OLD-1', 'explicit', now() - interval '1 day')`,
	`insert into issue_component_members (issue_key, project_key, component_id) values ('OLD-1', 'OLD', 'census')`,
}

// oneWriterRenamedFrom is the name renamedRows seed under, which their pair renames to
// one-writer/renamed-to.
const oneWriterRenamedFrom = "one-writer/renamed-from"

// renamedRows are the rows the boot's rename pass rewrites, as cmd/dispatch's test of the pass
// seeds them: the rename pair, a repo_projects row, and under its old name project RENAME's
// default repository, an issue's repository, a result's commit repository, and a delivery pull
// request with its URL and the issue URL it cites.
var renamedRows = []string{
	`insert into projects (key, name, default_assignee, default_repo) values ('RENAME', 'Rename', 'alice', 'one-writer/renamed-from')`,
	`insert into repo_projects (repo, project, created_by, renamed_to)
		values ('one-writer/renamed-from', 'RENAME', '{"kind":"user","id":"alice"}', 'one-writer/renamed-to')`,
	`insert into issues (key, project_key, number, title, created_by, rank, repo)
		values ('RENAME-1', 'RENAME', 1, 'One', '{"kind":"user","id":"alice"}', 'U', 'one-writer/renamed-from')`,
	`insert into artifacts (id, issue_key, project_key, slug, name, kind, created_by)
		values ('00000000-0000-4000-8000-00000000e001', 'RENAME-1', 'RENAME', 'verification', 'verification.md', 'doc', '{"kind":"user","id":"alice"}')`,
	`insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
			check_sha256, role, commit_sha, verdict, actor, commit_repo)
		values ('RENAME-1', 'RENAME-1', '00000000-0000-4000-8000-00000000e001', 'c1', 1, 'digest', 'tester', repeat('a', 40), 'pass',
			'{"kind":"user","id":"alice"}', 'one-writer/renamed-from')`,
	`insert into delivery_pull_requests (repo, number, title, url, author,
			attribution_title_keys, attribution_cited_issues, attribution_branch_keys, attribution_commit_keys)
		values ('one-writer/renamed-from', 11, 'Eleven', 'https://github.com/one-writer/renamed-from/pull/11', 'octocat',
			'{}', '{https://github.com/one-writer/renamed-from/issues/1}', '{}', '{}')`,
}

// componentCensus is the store census of each kind that components or component_depends projects
// on database, by its name.
func componentCensus(t *testing.T, database *store.Store) map[string]graphcensus.KindReport {
	t.Helper()
	report, err := graphcensus.StoreCensus(context.Background(), database.Pool.Config().ConnString(), false, nil)
	if err != nil {
		t.Fatalf("run the census: %v", err)
	}
	kinds := map[string]graphcensus.KindReport{}
	for _, kind := range report.Kinds {
		if table, _, _ := strings.Cut(kind.Table, "."); table == "components" || table == "component_depends" {
			kinds[kind.Kind] = kind
		}
	}
	return kinds
}

// ownDatabaseCases are the writers that run over a database of their own: the backfill of what a
// build before the store wrote, the seed command, the test corpus's loader and the boot's rename
// pass.
func ownDatabaseCases() []writerCase {
	ctx := context.Background()
	var seed string
	var components map[string]graphcensus.KindReport
	base := []writerCase{{
		name: "the backfill of what a build before the store wrote", on: "backfill", batches: true,
		tables: []string{"artifacts", "issues", "document_blocks", "block_refs", "comments", "asks", "ask_followers",
			"issue_links", "components", "component_depends", "issue_component_members"},
		ready: func(t *testing.T, r *oneWriterRig, database *store.Store) {
			for _, statement := range releaseBeforeRows {
				if _, err := database.Pool.Exec(ctx, statement); err != nil {
					t.Fatalf("write the rows a build before the store wrote: %v\n%s", err, statement)
				}
			}
		},
		drive: func(t *testing.T, r *oneWriterRig, database *store.Store) error {
			_, err := graphcensus.Backfill(ctx, database, io.Discard)
			return err
		},
	}, {
		name: "the seed command", on: "seed", fresh: true, batches: true, process: "cmd/dispatch-seed/",
		tables: []string{"issues", "artifacts", "comments", "asks", "components", "component_depends", "issue_links", "issue_component_members", "refs"},
		ready: func(t *testing.T, r *oneWriterRig, database *store.Store) {
			seed = cmdtest.BuildCommand(t, cmdtest.Command{Name: "dispatch-seed", Dir: filepath.Join(oneWriterModule, "cmd", "dispatch-seed")})
		},
		drive: func(t *testing.T, r *oneWriterRig, database *store.Store) error {
			output, err := exec.CommandContext(ctx, seed, "--database-url", database.Pool.Config().ConnString(), "--scale", "0.0002", "--no-migrate").CombinedOutput()
			if err != nil {
				return fmt.Errorf("dispatch-seed: %v: %s", err, lastLines(string(output), 5))
			}
			return nil
		},
	}, {
		name: "the test corpus's loader", on: "corpus", fresh: true, batches: true,
		tables: []string{"going_requests"},
		drive: func(t *testing.T, r *oneWriterRig, database *store.Store) error {
			fixtures, err := seedcorpus.FindFixtures(oneWriterRepositoryRoot(t))
			if err != nil {
				return err
			}
			_, err = seedcorpus.Seed(ctx, database, seedcorpus.Options{Fixtures: fixtures, TestCopy: true})
			return err
		},
	}, {
		// The pass writes no node and no tuple, and of a projection only issues.repo and
		// check_results.commit_repo, which the store does not state. Its collision delete of an
		// architecture_sources twin is read against the census of the components that source held.
		name: "the boot's rename pass", on: "rename", storeUnwritten: true,
		drives: []string{"internal/dispatch/store/repo_rename_pass.go#nameWrite"},
		ready: func(t *testing.T, r *oneWriterRig, database *store.Store) {
			for _, statement := range renamedRows {
				if _, err := database.Pool.Exec(ctx, statement); err != nil {
					t.Fatalf("write the rows under the old name: %v\n%s", err, statement)
				}
			}
			// Project RENAME's components, imported from its source under the old name with their
			// nodes and tuples, and then the source's twin under the new name, which the pass's
			// collision delete finds.
			const source = `insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
				values ('RENAME', $1, 'main', $2, '{"kind":"user","id":"alice"}')`
			if _, err := database.Pool.Exec(ctx, source, oneWriterRenamedFrom, oneWriterInstallation); err != nil {
				t.Fatalf("write project RENAME's source under the old name: %v", err)
			}
			if _, err := graphcensus.OneWriterImporter(t, database, oneWriterInstallation).Sync(ctx, "RENAME", oneWriterRenamedFrom); err != nil {
				t.Fatalf("import project RENAME's components from its source under the old name: %v", err)
			}
			if _, err := database.Pool.Exec(ctx, source, "one-writer/renamed-to", oneWriterInstallation); err != nil {
				t.Fatalf("write the source's twin under the new name: %v", err)
			}
			components = componentCensus(t, database)
			if len(components) == 0 {
				t.Fatal("the census names no kind that components or component_depends projects")
			}
			for name, kind := range components {
				if kind.Rows == 0 {
					t.Fatalf("the census of %s reads no row of the imported components, so the collision delete takes none", name)
				}
			}
		},
		// As the boot runs it (cmd/dispatch's renameRepositories): the pairs read once, and the
		// pass over them under the boot's bound.
		drive: func(t *testing.T, r *oneWriterRig, database *store.Store) error {
			pairs, err := store.ReadRepoRenames(ctx, database.Pool)
			if err != nil {
				return fmt.Errorf("read the rename pairs: %w", err)
			}
			bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			pass := store.RewriteRenamedRepositories(bounded, database.Pool, pairs)
			if err := pass.Err(); err != nil {
				return err
			}
			// Err is nil only where the pass left no row on the old name, so every seeded field holds
			// the new name. The one deletion is the source under the old name.
			if pass.Pairs != 1 || pass.Deleted != 1 {
				return fmt.Errorf("the pass did %+v, want one pair and one row deleted", pass)
			}
			return nil
		},
		check: func(t *testing.T, r *oneWriterRig, database *store.Store) {
			census := componentCensus(t, database)
			for _, name := range slices.Sorted(maps.Keys(census)) {
				kind, was := census[name], components[name]
				if kind.RowDifferences+kind.NodeDifferences > was.RowDifferences+was.NodeDifferences {
					t.Errorf("the boot's rename pass deleted the architecture_sources row under the old name for its twin under the new name (store.RewriteRenamedRepositories' collisions), "+
						"which took the row's components and component_depends rows with it through components_source_fkey's cascade and wrote no node and no tuple: "+
						"the census of %s reads %d rows and %d of the store's, %d row and %d node differences, where it read %d rows, %d row and %d node differences before the pass: %v %v",
						name, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences, was.Rows, was.RowDifferences, was.NodeDifferences, kind.ReportedRows, kind.ReportedNodes)
					continue
				}
				t.Logf("the census of %s reads %d rows and %d of the store's, %d differences, after the pass", name, kind.Rows, kind.Nodes, kind.RowDifferences+kind.NodeDifferences)
			}
		},
	}}
	return slices.Concat(base, storesMoveCases())
}

// seedStoresMoveCorpus writes the release before's priority, step and plan rows on database.
func seedStoresMoveCorpus(t *testing.T, database *store.Store) {
	t.Helper()
	storesmovetest.Write(t, database.Pool)
}

// storesMoveCases are the behavioural cases of the boot's move of the priority store and the
// workflow store into the graph store (storesmove.Move and storesmove/copy.go#writeBatch), one
// case per kind R32 moves: priority, step and plan.
//
// None of the three kinds has a separate projection table after R32 (census_stores.go): the store
// write is the tuples written to graph_tuples, graph_nodes holds the node-row projection
// (Node.Projection), and stores_moved_seqs is the move's record of the old seqs each batch copied.
// Injecting a failure on stores_moved_seqs ('between') and at the store write's commit ('after')
// proves each failing batch leaves no node, no tuple, no record row and no notification, and the
// plain run moves the kind into the store and passes the store census with zero differences.
func storesMoveCases() []writerCase {
	ctx := context.Background()
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	who := model.Actor{Kind: "user", ID: "alice", Proven: true}
	daemon := model.Actor{Kind: "session", ID: "session-one-writer-move"}
	tasks := json.RawMessage(`[{"id":"default","steps":[{"role":"planner"},{"role":"implementer"}]}]`)
	moveKind := func(kind string, seed func(t *testing.T, database *store.Store)) writerCase {
		return writerCase{
			name:    "the boot's move of " + kind + " (storesmove/copy.go#writeBatch)",
			on:      "stores-move-" + kind,
			tables:  []string{storesMovedRecord},
			process: "internal/dispatch/storesmove/",
			ready:   func(t *testing.T, _ *oneWriterRig, database *store.Store) { seed(t, database) },
			drive: func(t *testing.T, _ *oneWriterRig, database *store.Store) error {
				result, err := storesmove.Move(ctx, database.Pool.Config().ConnString(), storesmove.Options{Batch: 3})
				if err != nil {
					return err
				}
				if !result.Moved || result.Copied == 0 {
					return fmt.Errorf("storesmove.Move of %s answered %+v, want Moved and Copied > 0", kind, result)
				}
				return nil
			},
			check: func(t *testing.T, _ *oneWriterRig, database *store.Store) {
				report, err := graphcensus.StoreCensus(ctx, database.Pool.Config().ConnString(), false, nil)
				if err != nil {
					t.Fatalf("run the store census after the boot's move of %s: %v", kind, err)
				}
				checked := false
				for _, k := range report.Kinds {
					if k.Kind != kind {
						continue
					}
					checked = true
					if k.Rows == 0 || k.Nodes == 0 {
						t.Errorf("after the boot's move of %s, the census reads %d rows and %d nodes, want both above zero", kind, k.Rows, k.Nodes)
					}
					if k.RowDifferences+k.NodeDifferences != 0 {
						t.Errorf("after the boot's move of %s, the census reads %d row and %d node differences: %v %v",
							kind, k.RowDifferences, k.NodeDifferences, k.ReportedRows, k.ReportedNodes)
					} else {
						t.Logf("after the boot's move of %s, the census reads %d rows and %d nodes, zero differences", kind, k.Rows, k.Nodes)
					}
				}
				if !checked {
					t.Errorf("the store census did not report kind %s", kind)
				}
			},
		}
	}
	return []writerCase{
		moveKind("priority", func(t *testing.T, database *store.Store) {
			storesmovetest.Issue(t, database.Pool, "MOVEP", "MOVEP-1", 1)
			storesmovetest.CreatePriority(t, database.Pool, "move-goal-1",
				map[string]string{"title": "Move goal one", "owner": "alice", "rank": "a0"}, who, at)
			storesmovetest.CreatePriority(t, database.Pool, "move-goal-2",
				map[string]string{"title": "Move goal two", "rank": "a1"}, who, at.Add(time.Minute))
			storesmovetest.Advance(t, database.Pool, "MOVEP-1", "move-goal-1", true, who, at.Add(2*time.Minute))
		}),
		moveKind("step", func(t *testing.T, database *store.Store) {
			storesmovetest.Issue(t, database.Pool, "MOVES", "MOVES-1", 1)
			storesmovetest.PutNode(t, database.Pool, "step", "0f1e2d3c-0000-4000-8000-0000000001b1", "MOVES-1", []storesmovetest.Attribute{
				storesmovetest.Text("issue", "MOVES-1"), storesmovetest.Text("project", "MOVES"),
				{Name: "generation", Value: 1, Text: "1"}, storesmovetest.Text("task", "default"),
				{Name: "step", Value: 1, Text: "1"}, storesmovetest.Text("kind", "step"), storesmovetest.Text("role", "planner"),
				storesmovetest.Text("started_at", at.Format(time.RFC3339Nano)),
			}, nil, daemon, at)
		}),
		moveKind("plan", func(t *testing.T, database *store.Store) {
			storesmovetest.Issue(t, database.Pool, "MOVEPL", "MOVEPL-1", 1)
			storesmovetest.PutNode(t, database.Pool, "plan", "0f1e2d3c-0000-4000-8000-0000000001a1", "MOVEPL-1", []storesmovetest.Attribute{
				storesmovetest.Text("issue", "MOVEPL-1"), storesmovetest.Text("project", "MOVEPL"),
				{Name: "generation", Value: 1, Text: "1"}, storesmovetest.Text("source", "default"),
				storesmovetest.Text("artifact_id", ""), {Name: "version", Value: 0, Text: "0"},
				{Name: "tasks", Value: tasks, Text: string(tasks)},
			}, nil, daemon, at)
		}),
	}
}

// lastLines is the last count lines of text.
func lastLines(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.Join(lines[max(0, len(lines)-count):], "\n")
}

// decodedOr decodes body into a T, answering the zero value where it cannot.
func decodedOr[T any](body []byte) T {
	var value T
	_ = json.Unmarshal(body, &value)
	return value
}

// waitFor waits until query, a select of one boolean over the rig's database, answers true, and
// answers an error once it has waited within.
func (r *oneWriterRig) waitFor(query string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		var done bool
		if err := r.database.Pool.QueryRow(context.Background(), query).Scan(&done); err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not answer true within %s", strings.Join(strings.Fields(query), " "), within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
