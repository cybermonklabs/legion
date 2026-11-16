package docs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// writeAccess runs write in a transaction of its own and commits it, as an access route does: the
// commit is what announces the change to every process's room sync.
func writeAccess(t *testing.T, service *Service, write func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the access write: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := write(ctx, tx); err != nil {
		t.Fatalf("write the access change: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the access change: %v", err)
	}
}

// documentSocketRefusal is how the document socket at serverURL refuses login's upgrade to room:
// its status and its body.
func documentSocketRefusal(t *testing.T, serverURL, room, login string) string {
	t.Helper()
	url := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws/doc/" + room + "?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	connection, response, err := gws.DefaultDialer.Dial(url, http.Header{"X-Dispatch-User": []string{login}})
	if err == nil {
		_ = connection.Close()
		t.Fatalf("%s opened the document socket of %s; want it refused", login, room)
	}
	if response == nil {
		t.Fatalf("%s's upgrade to %s: %v, with no answer", login, room, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the refusal of %s's upgrade to %s: %v", login, room, err)
	}
	return response.Status + " " + string(body)
}

// TestTheDocumentSocketClosesWhenItsPersonLosesTheProject: a document of a private project is a
// room only the project's people open. carol, whom no list names, is refused it exactly as a room
// no document holds is refused; bob, on the access list, opens it, as does alice, its owner. Once
// alice takes bob off the list, the access change's announcement reaches the room sync, which
// closes bob's socket with the code on which the provider reconnects, and bob's reconnect is
// refused as carol's upgrade was. alice's socket stays open.
func TestTheDocumentSocketClosesWhenItsPersonLosesTheProject(t *testing.T) {
	const bob, carol = "bob@outside.example", "carol@example.com"
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	runRoomSync(t, newRoomSync(service))
	owner := model.Actor{Kind: "user", ID: "alice"}
	writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
		if _, _, err := access.SetVisibility(ctx, tx, service.events, "DOC", model.ProjectPrivate, owner); err != nil {
			return err
		}
		_, _, err := access.AddPrincipal(ctx, tx, service.events, "DOC", access.PrincipalPerson, bob, owner)
		return err
	})
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)

	missing := documentSocketRefusal(t, httpServer.URL, uuid.NewString(), carol)
	if hidden := documentSocketRefusal(t, httpServer.URL, artifactID, carol); hidden != missing {
		t.Fatalf("carol's upgrade to a document of DOC, which she does not see, is refused %q; a room no document holds is refused %q", hidden, missing)
	}
	listed := connectPeer(t, httpServer.URL, artifactID, bob)
	if err := listed.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
		t.Fatalf("bob, on DOC's list, reads its document: %v", err)
	}
	keeper := connectPeer(t, httpServer.URL, artifactID, "alice")
	if err := keeper.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
		t.Fatalf("alice, DOC's owner, reads its document: %v", err)
	}

	writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := access.RemovePrincipal(ctx, tx, service.events, "DOC", access.PrincipalPerson, bob, owner)
		return err
	})
	select {
	case <-listed.Ended:
	case <-time.After(roomWait(t)):
		t.Fatalf("bob's document socket was still open %s after he was taken off DOC's list", roomWait(t))
	}
	if code := listed.CloseCode(); code != contracts.DocumentAdmissionRevokedCloseCode {
		t.Errorf("bob's document socket closed with %d, want %d, on which the provider reconnects", code, contracts.DocumentAdmissionRevokedCloseCode)
	}
	if again := documentSocketRefusal(t, httpServer.URL, artifactID, bob); again != missing {
		t.Errorf("bob's reconnect after losing DOC is refused %q; a room no document holds is refused %q", again, missing)
	}
	// One pass of the room sync closes every socket it closes at once, so a second after bob's
	// close and his refusal is room enough for alice's to have ended with his.
	select {
	case <-keeper.Ended:
		t.Fatalf("alice's document socket closed (%d), though she still sees DOC", keeper.CloseCode())
	case <-time.After(time.Second):
	}
}

// TestASocketAdmittedAcrossAnAccessCheckIsClosed: an access re-check walks the sockets recorded when
// it begins, so one whose admission read its room before the re-check began and that is recorded
// only after it is closed as it is recorded, with the cause on which the provider reconnects; one
// whose admission read its room after the re-check began is recorded and kept.
func TestASocketAdmittedAcrossAnAccessCheckIsClosed(t *testing.T) {
	var admissions schemaAdmissions
	admitted := func(checks uint64) (*schemaAdmission, context.Context) {
		ctx, cancel := context.WithCancelCause(context.Background())
		t.Cleanup(func() { cancel(nil) })
		return &schemaAdmission{key: schemaAdmissionKey{room: uuid.NewString()}, person: "bob@outside.example", accessChecks: checks, cancel: cancel}, ctx
	}
	before, beforeCtx := admitted(admissions.accessCheckCount())
	if held := admissions.beginAccessCheck(); len(held) != 0 {
		t.Fatalf("the re-check found %v before any socket was recorded", held)
	}
	after, afterCtx := admitted(admissions.accessCheckCount())
	admissions.admit(before)
	admissions.admit(after)
	if cause := context.Cause(beforeCtx); cause != errAccessRevoked {
		t.Errorf("the socket that read its room before the re-check began ends with %v, want errAccessRevoked", cause)
	}
	if afterCtx.Err() != nil {
		t.Errorf("the socket that read its room after the re-check began was closed: %v", context.Cause(afterCtx))
	}
	if held := admissions.beginAccessCheck()["bob@outside.example"]; len(held) != 1 || held[0] != after {
		t.Errorf("the next re-check walks %v, want the kept socket alone", held)
	}
}
