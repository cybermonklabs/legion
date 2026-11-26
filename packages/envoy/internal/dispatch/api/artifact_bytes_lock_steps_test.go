package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestALaterVersionStatesItsBytesInTheStoreLockOrder holds statedVersionBytes (api/artifacts.go),
// the write of the bytes of an image's or a file's later version on the node its row names
// (the issue-tree spec), to the store's lock order, with the machinery
// TestEveryObjectNodeWriterTakesTheStoreLockOrder holds the creates with: a second version of an
// issue's image takes the issue's owner lock, then reads its own artifacts row under it, locking
// no row, then appends the artifact.version event, then the bytes under it.
//
// It then races the upload against the real backfill (graphcensus.Backfill) over a row an older
// build wrote, with no node, no marker and no version node: the backfill names the node on the row,
// and states no bytes there, since the version the row's newest is has no node of its own and its
// bytes are the version pass's to state with that node (nameArtifactNodes). It is held at the
// commit of that object pass, by a deferred trigger waiting on a lock the test holds, while the
// upload of the second version runs. Each upload waits for the backfill at the document's owner
// lock, which the backfill took first: an issue's image at the issue's, and a conversation's file,
// whose upload takes no owner lock of its own conversation, at the file's own, which
// statedVersionBytes takes before its read. Let go, the upload reads the node the backfill named and
// states its bytes there, and the version pass, which then finds the first version no longer the
// newest, states none: the node's bytes are the second version's alone.
// Without that lock the conversation's upload reads no node, states nothing, and the node
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
		name: "an issue's image waits at its issue's owner lock", route: "/api/v1/issues/" + rig.issue + "/artifacts",
		owner: "issue", kind: "image", mime: "image/png", waits: store.OwnerLockStatement,
		first: pngBytes("the older build's picture"), second: pngBytes("the picture uploaded while the backfill ran"),
	}, {
		name: "a conversation's file waits at its document's owner lock", route: "/api/v1/agents/" + session + "/artifacts",
		owner: "session", kind: "file", mime: "application/octet-stream", waits: store.OwnerLockStatement,
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
			if early, done := storetest.WaitForQueuedBehind(t, holder, backfillPID, backfilled); done {
				t.Fatalf("the backfill answered (%v) without reaching its commit over %s", early, refKey)
			}

			backend := rig.callBackend(t, ctx)
			uploaded := make(chan error, 1)
			go func() {
				uploaded <- statusOrError(multipartRequest(t, rig.call, race.route, map[string]string{"name": name},
					name, race.mime, race.second, "alice"), http.StatusCreated)
			}()
			rig.waitQueued(t, ctx, holder, backend, uploaded, "the later version of "+refKey+" behind the backfill")
			if running, cut := rig.runningStatement(t, ctx, backend); !runsStatement(running, cut, race.waits) {
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
// (removeArtifact), which states the document's node gone (removedDocumentNode, the issue-tree
// spec), to the store's lock order with the same machinery: the issue's owner lock, then the node,
// then the delete of the document's row, then the artifact.removed event and the state it caused
// (documentRemoval, which TestEveryObjectNodeWriterTakesTheStoreLockOrder runs too). The room's
// lock the removal takes after the issue's owner lock is an advisory lock, which no table step
// shows.
func TestARemovalStatesItsDocumentGoneInTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	rig := newStoreWriterRig(t)
	writer := rig.documentRemoval()
	requireTheWrittenOrderAdmits(t, writer)
	requireEachStepIsWaitedAtInTurn(t, ctx, rig, writer)
}

// laterImageVersion is the upload of an issue's image's second version: the issue's owner lock,
// which statedVersionBytes takes again before it reads the image's own artifacts row, which it
// locks no row of, the artifact.version event and the bytes it caused. The bytes are tuples on the
// node the row names and the write writes no node, so they are graphstore's plain tuple append
// (plainTupleAppendStep). Each run's image is uploaded afresh, at its first version, before the
// run.
func (r *storeWriterRig) laterImageVersion() storeWriter {
	route := "/api/v1/issues/" + r.issue + "/artifacts"
	return storeWriter{
		name: "statedVersionBytes on an issue's image",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			eventAppendStep(),
			plainTupleAppendStep(),
		},
		ready: func(t *testing.T, _ context.Context) storeWriterRun {
			name := r.fresh("lock-shot") + ".png"
			first := multipartRequest(t, r.handler, route, map[string]string{"name": name}, name, "image/png",
				pngBytes(name+" at its first version"), "alice")
			if first.Code != http.StatusCreated {
				t.Fatalf("upload the first version of %s: status=%d body=%s", name, first.Code, first.Body.String())
			}
			return storeWriterRun{write: func() error {
				return statusOrError(multipartRequest(t, r.call, route, map[string]string{"name": name}, name, "image/png",
					pngBytes(name+" at its second version"), "alice"), http.StatusCreated)
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
