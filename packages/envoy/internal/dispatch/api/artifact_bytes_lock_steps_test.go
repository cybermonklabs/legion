package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// versionBytesStatement is statedVersionBytes' read of the node an image's or a file's row names,
// which takes that row FOR SHARE.
const versionBytesStatement = "select node_address from artifacts where id = $1 for share"

// TestALaterVersionStatesItsBytesInTheStoreLockOrder holds statedVersionBytes (api/artifacts.go),
// the write of the bytes of an image's or a file's later version on the node its row names
// (dispatch://INFRA-1253), to the store's lock order, with the machinery
// TestEveryObjectNodeWriterTakesTheStoreLockOrder holds the creates with: a second version of an
// issue's image takes the issue's row, then its own artifacts row FOR SHARE, then appends the
// artifact.version event, then the bytes under it.
//
// It then races the upload against the real backfill (graphcensus.Backfill) over a row an older
// build wrote, with no node, no marker and no version node: the backfill names the node on the row,
// and states no bytes there, since the version the row's newest is has no node of its own and its
// bytes are the version pass's to state with that node (nameArtifactNodes). It is held at the
// commit of that object pass, by a deferred trigger waiting on a lock the test holds, while the
// upload of the second version runs. An issue's image waits for the backfill at its owner row, the
// issue's, which the backfill took first; a conversation's file, whose upload takes no owner row,
// waits at its artifacts row. Let go, the upload reads the node the backfill named and states its
// bytes there, and the version pass, which then finds the first version no longer the newest,
// states none: the node's bytes are the second version's alone.
// Without the FOR SHARE the conversation's upload reads no node, states nothing, and the node
// holds no bytes for good.
func TestALaterVersionStatesItsBytesInTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	rig := newStoreWriterRig(t)
	t.Run("statedVersionBytes on an issue's image", func(t *testing.T) {
		writer := rig.laterImageVersion()
		requireTheWrittenOrderAdmits(t, writer)
		requireEachStepIsWaitedAtInTurn(t, ctx, rig, writer)
	})

	const waitLock = 31_337_253
	if _, err := rig.database.Pool.Exec(ctx, `
		create or replace function test_wait_at_commit() returns trigger language plpgsql as $$
		begin
		  perform pg_advisory_xact_lock(`+strconv.Itoa(waitLock)+`);
		  return null;
		end $$;
		create constraint trigger zz_wait_at_commit after update of node_kind on artifacts
		deferrable initially deferred
		for each row execute function test_wait_at_commit();
	`); err != nil {
		t.Fatalf("install the trigger that holds the backfill at its commit: %v", err)
	}
	t.Cleanup(func() {
		_, _ = rig.database.Pool.Exec(context.Background(), `drop trigger if exists zz_wait_at_commit on artifacts`)
	})
	session := uuid.NewString()
	for _, race := range []struct {
		name, route, owner, kind, mime, waits string
		first, second                         []byte
	}{{
		name: "an issue's image waits at its issue's row", route: "/api/v1/issues/" + rig.issue + "/artifacts",
		owner: "issue", kind: "image", mime: "image/png", waits: issueOwnerStatement,
		first: pngBytes("the older build's picture"), second: pngBytes("the picture uploaded while the backfill ran"),
	}, {
		name: "a conversation's file waits at its artifacts row", route: "/api/v1/agents/" + session + "/artifacts",
		owner: "session", kind: "file", mime: "application/octet-stream", waits: versionBytesStatement,
		first: []byte("\x00\x01 the older build's trace"), second: []byte("\x00\x02 the trace uploaded while the backfill ran"),
	}} {
		t.Run(race.name, func(t *testing.T) {
			name := rig.fresh("raced") + ".bin"
			if race.kind == "image" {
				name = rig.fresh("raced") + ".png"
			}
			refKey := seedOlderVersionedFile(t, rig, race.owner, session, name, race.kind, race.mime, race.first)

			// The backfill's connection is made before the holder, so the holder's release, a
			// cleanup too, runs first and a failed assertion never leaves the backfill's pool closing
			// over a run still waiting on the holder's lock.
			backfiller := storetest.SingleConnection(t, rig.database)
			var backfillPID uint32
			if err := backfiller.Pool.QueryRow(ctx, `select pg_backend_pid()`).Scan(&backfillPID); err != nil {
				t.Fatalf("read the backfill's backend: %v", err)
			}
			holder, release := rig.beginHolder(t, ctx)
			if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock($1)`, waitLock); err != nil {
				t.Fatalf("take the lock the backfill's commit waits on: %v", err)
			}
			backfilled := make(chan error, 1)
			go func() {
				_, err := graphcensus.Backfill(context.Background(), backfiller, io.Discard)
				backfilled <- err
			}()
			if early, done := storetest.WaitForQueuedBehind(t, holder, backfillPID, backfilled, storeStepWait); done {
				t.Fatalf("the backfill answered (%v) without reaching its commit over %s", early, refKey)
			}

			backend := rig.callBackend(t, ctx)
			uploaded := make(chan error, 1)
			go func() {
				uploaded <- createdOrError(multipartRequest(t, rig.call, race.route, map[string]string{"name": name},
					name, race.mime, race.second, "alice"))
			}()
			rig.waitQueued(t, ctx, holder, backend, uploaded, "the later version of "+refKey+" behind the backfill")
			if running := rig.runningStatement(t, ctx, backend); !strings.HasPrefix(running, normalizedStatement(race.waits)) {
				t.Errorf("the later version of %s waits behind the backfill at %q, want %q", refKey, running, race.waits)
			}
			release()
			for what, answered := range map[string]<-chan error{"the backfill": backfilled, "the upload": uploaded} {
				select {
				case err := <-answered:
					if err != nil {
						t.Fatalf("%s, once the backfill's commit was let go: %v", what, err)
					}
				case <-time.After(storeStepWait):
					t.Fatalf("%s did not finish within %s of the backfill's commit being let go", what, storeStepWait)
				}
			}

			secondSum := sha256.Sum256(race.second)
			for attribute, value := range map[string]string{
				graphstore.AttributeMIME:   race.mime,
				graphstore.AttributeSize:   strconv.Itoa(len(race.second)),
				graphstore.AttributeSHA256: hex.EncodeToString(secondSum[:]),
			} {
				history, err := graphstore.History(ctx, rig.database.Pool, graphstore.KindArtifact, refKey, attribute)
				if err != nil {
					t.Fatalf("read the %s of %s: %v", attribute, refKey, err)
				}
				if len(history) != 1 || history[0].ToID != value {
					t.Errorf("%s states %s %+v, want the second version's %q alone", refKey, attribute, history, value)
				}
				current, held, err := graphstore.CurrentValue(ctx, rig.database.Pool, graphstore.KindArtifact, refKey, attribute)
				if err != nil || !held || current != value {
					t.Errorf("%s's %s is %q (held %t, %v), want the second version's %q", refKey, attribute, current, held, err, value)
				}
			}
		})
	}
}

// TestARemovalStatesItsDocumentGoneInTheStoreLockOrder holds the removal of an issue's document
// (removeArtifact), which states the document's node gone (removedDocumentNode, dispatch://INFRA-1253),
// to the store's lock order with the same machinery: the issue's row, then the node, then the
// delete of the document's row, then the artifact.removed event and the state it caused. The
// room's lock the removal takes after the issue's row is an advisory lock, which no table step
// shows.
func TestARemovalStatesItsDocumentGoneInTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	rig := newStoreWriterRig(t)
	writer := rig.documentRemoval()
	requireTheWrittenOrderAdmits(t, writer)
	requireEachStepIsWaitedAtInTurn(t, ctx, rig, writer)
}

// documentRemoval is the removal of an issue's untouched document: the issue's row, the document's
// node stated gone, the document's row deleted, the artifact.removed event and the state it
// caused. Each run's document is made afresh, by typing a title, before the run.
func (r *storeWriterRig) documentRemoval() storeWriter {
	route := "/api/v1/issues/" + r.issue + "/artifacts"
	return storeWriter{
		name: "removedDocumentNode in an issue document's removal",
		steps: []storeStep{
			r.issueOwnerStep(issueOwnerStatement),
			{
				name: "the document's node", order: nodeStep,
				hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			{
				name: "the document's row", order: projectionStep,
				hold: holdTableWrites("artifacts"), at: "delete from artifacts where id = $1",
				takes: []tableLock{{"artifacts", rowExclusiveMode}},
			},
			eventAppendStep(),
			tupleAppendStep(),
		},
		ready: func(t *testing.T, _ context.Context) storeWriterRun {
			made := dispatchRequest(t, r.handler, http.MethodPost, route, map[string]any{"name": r.fresh("lock-removed")}, "alice")
			if made.Code != http.StatusCreated {
				t.Fatalf("make a document to remove: status=%d body=%s", made.Code, made.Body.String())
			}
			document := decodeBody[artifactUploadResponse](t, made).Artifact.ID
			return storeWriterRun{write: func() error {
				response := dispatchRequest(t, r.call, http.MethodDelete, "/api/v1/artifacts/"+document, nil, "alice")
				if response.Code != http.StatusNoContent {
					return fmt.Errorf("status=%d body=%s", response.Code, response.Body.String())
				}
				return nil
			}}
		},
	}
}

// laterImageVersion is the upload of an issue's image's second version: the issue's row, the
// image's own artifacts row read FOR SHARE (statedVersionBytes), the artifact.version event and the
// bytes it caused. Each run's image is uploaded afresh, at its first version, before the run.
func (r *storeWriterRig) laterImageVersion() storeWriter {
	route := "/api/v1/issues/" + r.issue + "/artifacts"
	var image string
	return storeWriter{
		name: "statedVersionBytes on an issue's image",
		steps: []storeStep{
			r.issueOwnerStep(issueOwnerStatement),
			{
				name: "the image's row", order: projectionStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ storeWriterRun) {
					holdRow(t, ctx, holder, `select 1 from artifacts where id = $1::uuid for no key update`, image)
				},
				at:    versionBytesStatement,
				takes: []tableLock{{"artifacts", rowShareMode}},
			},
			eventAppendStep(),
			tupleAppendStep(),
		},
		ready: func(t *testing.T, _ context.Context) storeWriterRun {
			name := r.fresh("lock-shot") + ".png"
			first := multipartRequest(t, r.handler, route, map[string]string{"name": name}, name, "image/png",
				pngBytes(name+" at its first version"), "alice")
			if first.Code != http.StatusCreated {
				t.Fatalf("upload the first version of %s: status=%d body=%s", name, first.Code, first.Body.String())
			}
			image = decodeBody[artifactUploadResponse](t, first).Artifact.ID
			return storeWriterRun{write: func() error {
				return createdOrError(multipartRequest(t, r.call, route, map[string]string{"name": name}, name, "image/png",
					pngBytes(name+" at its second version"), "alice"))
			}}
		},
	}
}

// seedOlderVersionedFile writes, by SQL in the shape a build before the store wrote it, an image
// or a file of the rig's issue (owner "issue") or of the conversation session (owner "session"),
// and its first version: a row naming no node, and no node for it or for its version. It answers
// the row's ref_key.
func seedOlderVersionedFile(t *testing.T, rig *storeWriterRig, owner, session, name, kind, mime string, content []byte) string {
	t.Helper()
	ctx := context.Background()
	var issueKey, project, sessionID *string
	if owner == "issue" {
		issueKey, project = &rig.issue, new("LOCK")
	} else {
		sessionID = &session
	}
	var refKey string
	sum := sha256.Sum256(content)
	if err := rig.database.Pool.QueryRow(ctx, `
		with made as (
			insert into artifacts (issue_key, project_key, session_id, slug, name, kind, created_by)
			values ($1, $2, $3, $4, $5, $6, '{"kind":"user","id":"alice"}')
			returning id, ref_key
		)
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors)
		select made.id, 1, $7, $8, $9, $10, '[{"kind":"user","id":"alice"}]' from made
		returning (select ref_key from made)
	`, issueKey, project, sessionID, artifactSlug(name), name, kind, content, mime, len(content),
		hex.EncodeToString(sum[:])).Scan(&refKey); err != nil {
		t.Fatalf("seed %s as an older build wrote it: %v", name, err)
	}
	return refKey
}
