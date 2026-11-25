package docs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// docsocketHeartbeat is the heartbeat these rows run the access re-check on (RunAccessRecheck), in
// place of the API's fifteen seconds (api: Deps.StreamHeartbeat).
const docsocketHeartbeat = 200 * time.Millisecond

// docsocketPassSlack is what a close may take beyond one heartbeat after the change committed: the
// pass's two reads of the person (readableRooms) and ygo's close frame, on a loaded runner.
const docsocketPassSlack = 3 * time.Second

// runAccessHeartbeat runs service's access re-check on docsocketHeartbeat, as the API runs it on the
// document sockets' heartbeat, until the test ends, and waits for it to return before the
// service's own shutdown, which the test registered first.
func runAccessHeartbeat(t *testing.T, service *Service) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.RunAccessRecheck(ctx, docsocketHeartbeat)
	}()
	testcleanup.Add(t, func() {
		stop()
		<-done
	})
}

// awaitAccessPasses waits until passes more access re-checks have begun on service than when it was
// called, failing the test if any of open has ended meanwhile: a pass that leaves a socket open has
// to have run for that to say anything.
func awaitAccessPasses(t *testing.T, service *Service, passes uint64, open map[string]*docstest.Peer) {
	t.Helper()
	want := service.schemaAdmissions.accessCheckCount() + passes
	deadline := time.After(time.Duration(passes)*docsocketHeartbeat + docsocketPassSlack)
	for service.schemaAdmissions.accessCheckCount() < want {
		for login, peer := range open {
			select {
			case <-peer.Ended:
				t.Fatalf("%s's document socket closed (%d) while %s still reads its room", login, peer.CloseCode(), login)
			default:
			}
		}
		select {
		case <-deadline:
			t.Fatalf("%d access re-checks began within %s, want %d: the heartbeat runs no pass", service.schemaAdmissions.accessCheckCount()-(want-passes), time.Duration(passes)*docsocketHeartbeat+docsocketPassSlack, passes)
		case <-time.After(docsocketHeartbeat / 10):
		}
	}
	for login, peer := range open {
		select {
		case <-peer.Ended:
			t.Fatalf("%s's document socket closed (%d) while %s still reads its room", login, peer.CloseCode(), login)
		default:
		}
	}
}

// TestADocumentSocketClosesWithinAHeartbeatOfItsPersonLosingTheRoomBySQLAlone: an access change made
// by SQL alone, as a statement at a psql prompt makes it, appends no event and announces nothing on
// store.AccessChangedChannel, so the room sync that listens there, running here as it runs in every
// process, hears nothing of it. The access re-check the sockets' heartbeat runs
// (RunAccessRecheck) finds it all the same: the socket of the person who lost the room closes with
// the code an announced change closes it with, within a heartbeat and a pass of the change, and
// their reconnect is refused as a room no document holds is; alice, DOC's owner, keeps hers. The
// change is each kind a person loses a room by: bob, on private DOC's list, taken off it; and
// public DOC made private, which takes it from carol, a full member no list names. Before the
// change the heartbeat's passes run with both sockets open and close neither.
//
// Control: the heartbeat's loop runs no re-check; the losing socket stays open and the row fails
// at its wait.
func TestADocumentSocketClosesWithinAHeartbeatOfItsPersonLosingTheRoomBySQLAlone(t *testing.T) {
	const bob, carol = "bob@outside.example", "carol@example.com"
	for _, row := range []struct {
		name string
		// private makes DOC private with bob on its list, through the writes the access routes
		// make, before any socket opens.
		private bool
		// loser is the person the change takes the room from, and change the statement that does.
		loser, change string
	}{
		{
			name: "a person taken off the list", private: true, loser: bob,
			change: `delete from project_access where project_key = 'DOC' and principal_kind = 'person' and principal = '` + bob + `'`,
		},
		{
			name: "a public project made private", loser: carol,
			change: `update projects set visibility = 'private' where key = 'DOC'`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, "before")
			runRoomSync(t, newRoomSync(service))
			if row.private {
				owner := model.Actor{Kind: "user", ID: "alice"}
				writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
					if _, _, err := access.SetVisibility(ctx, tx, service.events, "DOC", model.ProjectPrivate, owner); err != nil {
						return err
					}
					_, _, err := access.AddPrincipal(ctx, tx, service.events, "DOC", access.PrincipalPerson, bob, owner)
					return err
				})
			}
			httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
			testcleanup.Add(t, httpServer.Close)
			runAccessHeartbeat(t, service)

			missing := documentSocketRefusal(t, httpServer.URL, uuid.NewString(), row.loser)
			losing := connectPeer(t, httpServer.URL, artifactID, row.loser)
			if err := losing.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
				t.Fatalf("%s reads DOC's document before the change: %v", row.loser, err)
			}
			keeper := connectPeer(t, httpServer.URL, artifactID, "alice")
			if err := keeper.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
				t.Fatalf("alice, DOC's owner, reads its document: %v", err)
			}
			awaitAccessPasses(t, service, 2, map[string]*docstest.Peer{row.loser: losing, "alice": keeper})

			if _, err := service.store.Pool.Exec(context.Background(), row.change); err != nil {
				t.Fatalf("%s by SQL alone: %v", row.name, err)
			}
			changed := time.Now()
			select {
			case <-losing.Ended:
				t.Logf("%s's document socket closed %s after the change committed (heartbeat %s)", row.loser, losing.EndedAt().Sub(changed).Round(time.Millisecond), docsocketHeartbeat)
			case <-time.After(docsocketHeartbeat + docsocketPassSlack):
				t.Fatalf("%s's document socket was still open %s after %s by SQL alone (heartbeat %s)", row.loser, docsocketHeartbeat+docsocketPassSlack, row.name, docsocketHeartbeat)
			}
			if code := losing.CloseCode(); code != contracts.DocumentAdmissionRevokedCloseCode {
				t.Errorf("%s's document socket closed with %d, want %d, as an announced access change closes it", row.loser, code, contracts.DocumentAdmissionRevokedCloseCode)
			}
			if again := documentSocketRefusal(t, httpServer.URL, artifactID, row.loser); again != missing {
				t.Errorf("%s's reconnect after %s is refused %q; a room no document holds is refused %q", row.loser, row.name, again, missing)
			}
			awaitAccessPasses(t, service, 2, map[string]*docstest.Peer{"alice": keeper})
		})
	}
}

// TestDocumentSocketsOutliveTheAccessHeartbeatWithNoPrivateProjectAndNoChange: in a deployment that
// holds no private project, and with no access change at all, the access re-check the sockets'
// heartbeat runs (RunAccessRecheck) reads each person's access again and closes nothing (R9):
// alice's and carol's sockets to public DOC stay open across several passes, which the count of
// re-checks begun shows have run, and each is still served: the room answers its next ask for the
// document.
func TestDocumentSocketsOutliveTheAccessHeartbeatWithNoPrivateProjectAndNoChange(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, httpServer.Close)
	runAccessHeartbeat(t, service)
	var private bool
	if err := service.store.Pool.QueryRow(context.Background(), `select exists (select 1 from projects where visibility = 'private')`).Scan(&private); err != nil || private {
		t.Fatalf("the deployment holds a private project (%t, %v); this row is for one that holds none", private, err)
	}

	open := map[string]*docstest.Peer{}
	for _, login := range []string{"alice", "carol@example.com"} {
		peer := connectPeer(t, httpServer.URL, artifactID, login)
		if err := peer.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
			t.Fatalf("%s reads public DOC's document: %v", login, err)
		}
		open[login] = peer
	}
	awaitAccessPasses(t, service, 5, open)
	for login, peer := range open {
		for drained := false; !drained; {
			select {
			case <-peer.Answers:
			default:
				drained = true
			}
		}
		if err := peer.AskForDocument(); err != nil {
			t.Fatalf("%s's socket, open across the heartbeat's passes, asks for the document: %v", login, err)
		}
		select {
		case <-peer.Answers:
		case <-peer.Ended:
			t.Fatalf("%s's socket closed (%d) instead of answering its ask for the document", login, peer.CloseCode())
		case <-time.After(roomWait(t)):
			t.Fatalf("%s's socket, open across the heartbeat's passes, was not answered its ask for the document within %s", login, roomWait(t))
		}
	}
}
