package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// An upload that supersedes a person's approval says so even when the issue's advice cannot be
// read in time. The warning is computed inside the upload's own transaction; only the issue-scoped
// advice beside it is a read of the pool after the commit, bounded at adviceAllowance. Here that
// read cannot finish: once the upload's transaction holds its lock on the asks table, another
// connection queues an exclusive lock behind it, which Postgres grants the moment the upload
// commits and before the advice's read of asks (your_open_asks), and holds it for longer than the
// allowance. The upload still lands, and its answer must still carry superseded_approval, as an
// edit's does when its advice is lost.
func TestAnUploadSupersedingAnApprovalSaysSoWhenItsAdviceIsUnavailable(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createAdviceProject(t, handler, "UPLOST")
	spec := "# Transport\n\nThe design, as approved.\n"
	issue := createAdviceIssue(t, handler, "UPLOST", "Approved spec", &spec)
	approved := approveAdviceDocument(t, handler, issue.PrimaryArtifactID)
	// Nothing else touches asks once the document has settled, so the lock found below is the
	// upload's.
	awaitSettled(t, database, issue.PrimaryArtifactID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	holder, err := pgx.Connect(ctx, database.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the holding connection: %v", err)
	}
	defer holder.Close(context.Background())
	queued := make(chan error, 1)
	go func() {
		// Wait for another backend to hold a lock on asks: the upload's transaction.
		for ctx.Err() == nil {
			var held bool
			if err := holder.QueryRow(ctx, `
				select exists (
					select 1 from pg_locks l join pg_class c on c.oid = l.relation
					where c.relname = 'asks' and l.granted and l.pid <> pg_backend_pid()
				)`).Scan(&held); err != nil {
				queued <- err
				return
			}
			if held {
				break
			}
			time.Sleep(time.Millisecond)
		}
		hold, err := holder.Begin(ctx)
		if err != nil {
			queued <- err
			return
		}
		// Granted only once the upload's transaction ends; held past the advice's allowance.
		if _, err := hold.Exec(ctx, `lock table asks in access exclusive mode`); err != nil {
			queued <- err
			return
		}
		time.Sleep(3 * adviceAllowance)
		queued <- hold.Rollback(context.Background())
	}()

	replaced := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": adviceSessionActor("session-editor"),
		"name":  "spec.md", "content": "# Transport\n\nThe design, rewritten.\n",
	})
	if err := <-queued; err != nil {
		t.Fatalf("hold the asks table behind the upload: %v", err)
	}
	if replaced.Code != http.StatusCreated {
		t.Fatalf("upload: status=%d body=%s", replaced.Code, replaced.Body.String())
	}
	var body struct {
		Advice *struct {
			SupersededApproval *struct {
				ApprovedVersion int `json:"approved_version"`
				Version         int `json:"version"`
			} `json:"superseded_approval"`
		} `json:"advice"`
	}
	if err := json.Unmarshal(replaced.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the upload's answer: %v\n%s", err, replaced.Body.String())
	}
	if body.Advice == nil || body.Advice.SupersededApproval == nil {
		t.Fatalf("the upload superseded the approval of version %d, and its answer does not say so: %s",
			approved, replaced.Body.String())
	}
	if got := body.Advice.SupersededApproval; got.ApprovedVersion != approved || got.Version != approved+1 {
		t.Fatalf("superseded_approval = %+v, want version %d superseding the approval of %d", *got, approved+1, approved)
	}
}
