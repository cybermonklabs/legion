package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// socketAskReads counts the statements a document socket's ask sends (socketStillReads): the
// rule's read of the person's rooms (readableRoomsStatement) and the read of whether a project is
// private (access.AnyProjectPrivate). It is installed on the store the service is built on, so it
// sees every connection the service's pool opens. While fail is set it fails each of them, as a
// database that does not answer would: it hands pgx a context already ended, so the statement is
// never sent, while the caller's own context stands; failed counts those.
type socketAskReads struct {
	rule, private, failed atomic.Int64
	fail                  atomic.Bool
}

func (r *socketAskReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	switch {
	case data.SQL == readableRoomsStatement:
		r.rule.Add(1)
	case strings.Contains(data.SQL, "select exists (select 1 from projects where visibility = 'private')"):
		r.private.Add(1)
	default:
		return ctx
	}
	if !r.fail.Load() {
		return ctx
	}
	r.failed.Add(1)
	failed, cancel := context.WithCancelCause(ctx)
	cancel(errors.New("the database does not answer the socket's read"))
	return failed
}

func (*socketAskReads) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// createSessionDocument creates a document of session's conversation, which belongs to no project
// and is read by the session content rule (access.DocumentClause through access.PartyClause), with
// markdown as its first version.
func createSessionDocument(t *testing.T, database *store.Store, session, markdown string) string {
	t.Helper()
	ctx := context.Background()
	var artifactID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (session_id, slug, name, kind, created_by)
		values ($1, 'notes-md', 'notes.md', 'doc', jsonb_build_object('kind', 'session', 'id', $1::text))
		returning id::text
	`, session).Scan(&artifactID); err != nil {
		t.Fatalf("create the session's document: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, $2, jsonb_build_array(jsonb_build_object('kind', 'session', 'id', $3::text)))
	`, artifactID, markdown, session); err != nil {
		t.Fatalf("create the session's document's first version: %v", err)
	}
	return artifactID
}

// makePrivateProject creates project key, private, with listed on its access list, through the
// writes the access routes make.
func makePrivateProject(t *testing.T, service *Service, key, listed string) {
	t.Helper()
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values ($1, $1, 'alice') on conflict (key) do nothing
	`, key); err != nil {
		t.Fatalf("create project %s: %v", key, err)
	}
	owner := model.Actor{Kind: "user", ID: "alice"}
	writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
		if _, _, err := access.SetVisibility(ctx, tx, service.events, key, model.ProjectPrivate, owner); err != nil {
			return err
		}
		_, _, err := access.AddPrincipal(ctx, tx, service.events, key, access.PrincipalPerson, listed, owner)
		return err
	})
}

// awaitEpochMove waits until the service's access epoch has moved past from: the notification of
// a write committed before the call has reached the process's listener.
func awaitEpochMove(t *testing.T, service *Service, from uint64) {
	t.Helper()
	deadline := time.Now().Add(roomWait(t))
	for service.accessEpoch.moves.Load() == from {
		if time.Now().After(deadline) {
			t.Fatalf("the access epoch did not move from %d within %s: no notification reached the listener", from, roomWait(t))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestADocumentSocketEndsAtTheFirstFrameAfterASessionWriteIsAnnounced: a write that takes a
// session's documents from a person without any change of a project's access - a tie of the
// session to another person (access.TieSession), or a private project its token listed
// (access.RecordListedReach) - announces itself on events.SessionAccessChannel as it commits, the
// process's listener moves the access epoch on it, and the room's next frame to the person asks
// the rule again and closes the socket with the code a revoked admission gets, before that frame
// reaches them. The room sync runs as in every process; no access heartbeat runs, and neither
// write announces on store.AccessChangedChannel, so only the socket's own ask can close it. The
// session is tied to dora, on no private project's list, and wrote in public PUB, which places it
// for an open viewer while a project is private (access.SessionClause); carol, a full member no
// list names, and bob, on private PRIV's list, hold sockets to its document. The write ties the
// session to PRIV for carol: she loses it, bob keeps it, and bob types.
//
// Control, per write: the same rows written by SQL alone, which announces nothing. The epoch does
// not move, carol's socket asks nothing before bob's frame, and her copy holds his edit: so
// between notifications a socket reads nothing, and the notification is what makes it read.
func TestADocumentSocketEndsAtTheFirstFrameAfterASessionWriteIsAnnounced(t *testing.T) {
	t.Parallel()
	const bob, carol, session = "bob@outside.example", "carol@example.com", "session-epoch-0123456789"
	for _, row := range []struct {
		name string
		// write announces the change; silent writes the same rows by SQL alone.
		write  func(ctx context.Context, q access.Querier) error
		silent string
	}{
		{
			name:   "a tie to a person on a private project",
			write:  func(ctx context.Context, q access.Querier) error { return access.TieSession(ctx, q, session, bob) },
			silent: `insert into session_owners (session_id, login) values ('` + session + `', '` + bob + `')`,
		},
		{
			name: "a private project its token lists",
			write: func(ctx context.Context, q access.Querier) error {
				return access.RecordListedReach(ctx, q, session, []string{"PRIV"})
			},
			silent: `insert into session_reached_projects (session_id, project_key) values ('` + session + `', 'PRIV')`,
		},
	} {
		for _, announced := range []bool{true, false} {
			name := row.name + "/announced"
			if !announced {
				name = row.name + "/control: by SQL alone"
			}
			t.Run(name, func(t *testing.T) {
				database := storetest.Open(t)
				artifactID := createSessionDocument(t, database, session, "# Notes")
				service := newTestServiceOn(t, database)
				ctx := context.Background()
				// PRIV is private before the session is tied to anyone, so its switch records no
				// project the session reached.
				makePrivateProject(t, service, "PRIV", bob)
				for _, statement := range []string{
					`insert into session_owners (session_id, login) values ('` + session + `', 'dora@example.com')`,
					`insert into projects (key, name, default_assignee) values ('PUB', 'Public', 'alice')`,
					`insert into session_projects (session_id, project_key) values ('` + session + `', 'PUB')`,
					`insert into session_projects_filled (filled_at) values (now())`,
				} {
					if _, err := database.Pool.Exec(ctx, statement); err != nil {
						t.Fatalf("%s: %v", statement, err)
					}
				}
				seedServiceText(t, service, artifactID, "before")
				runRoomSync(t, newRoomSync(service))
				httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
				testcleanup.Add(t, httpServer.Close)

				losing := connectPeer(t, httpServer.URL, artifactID, carol)
				if err := losing.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
					t.Fatalf("carol reads the session's document while only alice is tied to it: %v", err)
				}
				editor := connectPeer(t, httpServer.URL, artifactID, bob)
				if err := editor.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
					t.Fatalf("bob, on PRIV's list, reads the session's document: %v", err)
				}
				// carol's socket asks once, before the first frame after her admission; this frame
				// is that one, so the frame after the write asks only for the reason under test.
				typeOver(t, editor, "before", "between")
				if err := losing.WaitForText(peerMarkdown, "between\n", roomWait(t)); err != nil {
					t.Fatalf("carol's copy holds bob's first edit before the write: %v", err)
				}

				from := service.accessEpoch.moves.Load()
				if announced {
					if err := row.write(ctx, database.Pool); err != nil {
						t.Fatalf("%s: %v", row.name, err)
					}
					awaitEpochMove(t, service, from)
				} else if _, err := database.Pool.Exec(ctx, row.silent); err != nil {
					t.Fatalf("%s by SQL alone: %v", row.name, err)
				}
				typeOver(t, editor, "between", "after")
				if err := editor.WaitForText(peerMarkdown, "after\n", roomWait(t)); err != nil {
					t.Fatalf("bob's own copy holds his edit: %v", err)
				}
				if !announced {
					if err := losing.WaitForText(peerMarkdown, "after\n", roomWait(t)); err != nil {
						t.Fatalf("control: carol's copy does not hold bob's edit after %s by SQL alone (%v): the frame was asked about with no notification", row.name, err)
					}
					if moved := service.accessEpoch.moves.Load(); moved != from {
						t.Fatalf("control: the access epoch moved from %d to %d with no notification sent", from, moved)
					}
					return
				}
				select {
				case <-losing.Ended:
				case <-editor.Ended:
					t.Fatalf("bob's document socket closed (%d) while he still reads the session's document", editor.CloseCode())
				case <-time.After(roomWait(t)):
					t.Fatalf("carol's document socket was still open after %s was announced and bob typed: the frame went out unasked", row.name)
				}
				if code := losing.CloseCode(); code != contracts.DocumentAdmissionRevokedCloseCode {
					t.Errorf("carol's document socket closed with %d, want %d, as a revoked admission closes", code, contracts.DocumentAdmissionRevokedCloseCode)
				}
				if text := peerMarkdown(losing); strings.Contains(text, "after") {
					t.Errorf("carol's copy holds %q: a frame of the room reached her after %s was announced", text, row.name)
				}
			})
		}
	}
}

// socketStream is a room of two people, typist and reader, each with a document socket to one
// document, typing frames with no access change, for counting what the sockets' asks read.
type socketStream struct {
	service *Service
	reads   *socketAskReads
	typist  *docstest.Peer
	reader  *docstest.Peer
	text    string
}

// openSocketStream opens a socket stream on DOC's document; private makes PRIV private first, and
// roomSync runs the process's room sync (and with it the listener the access epoch hears on).
func openSocketStream(t *testing.T, private, roomSync bool) *socketStream {
	t.Helper()
	reads := &socketAskReads{}
	database := storetest.TracedStore(t, storetest.Open(t), reads)
	artifactID := createDocument(t, database, "# First")
	service := newTestServiceOn(t, database)
	if private {
		makePrivateProject(t, service, "PRIV", "bob@outside.example")
	}
	seedServiceText(t, service, artifactID, "before")
	if roomSync {
		runRoomSync(t, newRoomSync(service))
	}
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, httpServer.Close)
	stream := &socketStream{service: service, reads: reads, text: "before"}
	stream.reader = connectPeer(t, httpServer.URL, artifactID, "carol@example.com")
	if err := stream.reader.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
		t.Fatalf("carol reads public DOC's document: %v", err)
	}
	stream.typist = connectPeer(t, httpServer.URL, artifactID, "alice")
	if err := stream.typist.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
		t.Fatalf("alice reads DOC's document: %v", err)
	}
	// The first frames after each admission ask, by design; these keystrokes are those frames.
	stream.typeFrames(t, 2)
	return stream
}

// typeFrames types n keystrokes on the typist's socket, each with a cursor (awareness) update,
// and waits until the reader's copy holds the last.
func (s *socketStream) typeFrames(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		next := s.text + "x"
		typeOver(t, s.typist, s.text, next)
		if err := s.typist.SetAwareness(7, map[string]any{"cursor": i}); err != nil {
			t.Fatalf("send the typist's cursor: %v", err)
		}
		s.text = next
	}
	if err := s.reader.WaitForText(peerMarkdown, s.text+"\n", roomWait(t)); err != nil {
		t.Fatalf("the reader's copy holds the typist's %d keystrokes: %v", n, err)
	}
	if err := s.reader.WaitForAwareness(roomWait(t), func(states map[uint64]json.RawMessage) bool {
		return string(states[7]) == fmt.Sprintf(`{"cursor":%d}`, n-1)
	}); err != nil {
		t.Fatalf("the reader holds the typist's last cursor: %v", err)
	}
}

// TestADocumentSocketReadsNothingForFramesWithNoAccessChange: while the process's listener stands
// and nothing announces an access change, a document socket's data frames - document updates and
// cursor (awareness) updates alike - read nothing: neither the rule (while a project is private)
// nor whether a project is private (R9, an open person while none is). Twenty keystrokes with a
// cursor update each go out to carol with no read at all.
//
// Control, per deployment: the same stream with no room sync, so no LISTEN stands and the epoch
// keeps no answer; every data frame reads again, at least one read per keystroke.
func TestADocumentSocketReadsNothingForFramesWithNoAccessChange(t *testing.T) {
	t.Parallel()
	const keystrokes = 20
	for _, row := range []struct {
		name    string
		private bool
		// counted is the read each frame's ask makes in this deployment.
		counted func(*socketAskReads) int64
	}{
		{name: "a project is private: the rule", private: true, counted: func(r *socketAskReads) int64 { return r.rule.Load() }},
		{name: "no project is private (R9): whether one is", counted: func(r *socketAskReads) int64 { return r.private.Load() }},
	} {
		for _, listening := range []bool{true, false} {
			name := row.name + "/listening"
			if !listening {
				name = row.name + "/control: no listener"
			}
			t.Run(name, func(t *testing.T) {
				stream := openSocketStream(t, row.private, listening)
				before := row.counted(stream.reads)
				stream.typeFrames(t, keystrokes)
				read := row.counted(stream.reads) - before
				t.Logf("%d keystrokes, each with a cursor update, read %d times", keystrokes, read)
				if listening && read != 0 {
					t.Errorf("%d keystrokes with no access change read %d times; want none", keystrokes, read)
				}
				if !listening && read < keystrokes {
					t.Errorf("control: with no listener, %d keystrokes read %d times; want at least one per keystroke, so the count sees the reads", keystrokes, read)
				}
			})
		}
	}
}

// TestTheAccessEpochReadsWhetherAProjectIsPrivateOncePerMove: every open socket of a process that
// asks at one epoch shares one read of whether a project is private (anyProjectPrivate): ten asks
// at one epoch read once, and a move of the epoch reads once more.
//
// Control: with no LISTEN standing the epoch keeps no answer, and each of the ten reads.
func TestTheAccessEpochReadsWhetherAProjectIsPrivateOncePerMove(t *testing.T) {
	t.Parallel()
	for _, listening := range []bool{true, false} {
		t.Run(map[bool]string{true: "listening", false: "control: no listener"}[listening], func(t *testing.T) {
			reads := &socketAskReads{}
			service := newTestServiceOn(t, storetest.TracedStore(t, storetest.Open(t), reads))
			service.accessEpoch.listening.Store(listening)
			ctx := context.Background()
			ask := func() {
				var wg sync.WaitGroup
				for range 10 {
					wg.Go(func() {
						if private, err := service.anyProjectPrivate(ctx); err != nil || private {
							t.Errorf("read whether a project is private: %t, %v; want false", private, err)
						}
					})
				}
				wg.Wait()
			}
			ask()
			first := reads.private.Load()
			service.accessEpoch.move()
			ask()
			second := reads.private.Load() - first
			if listening && (first != 1 || second != 1) {
				t.Errorf("ten asks at one epoch read %d times, and ten after a move %d; want 1 and 1", first, second)
			}
			if !listening && (first != 10 || second != 10) {
				t.Errorf("control: with no listener ten asks read %d times, then %d; want 10 and 10", first, second)
			}
		})
	}
}

// TestAnOpenPersonsSocketOutlivesAFailedReadWhileNoProjectIsPrivate: in a deployment that holds no
// private project (R9), a read of whether one is private that fails passes an open person's frames
// on the last answer the socket read, so a database that does not answer once closes no socket:
// the epoch moves, the read fails, and carol's copy still takes alice's keystrokes on a socket that
// stays open. A read that answers again is asked at the next move.
//
// Control: the same failing reads while a project is private, where the rule decides: its read
// fails, and carol's socket closes with the code a revoked admission gets (it fails closed), and
// her copy never holds the keystroke.
func TestAnOpenPersonsSocketOutlivesAFailedReadWhileNoProjectIsPrivate(t *testing.T) {
	t.Parallel()
	for _, private := range []bool{false, true} {
		t.Run(map[bool]string{false: "no project is private (R9)", true: "control: a project is private"}[private], func(t *testing.T) {
			stream := openSocketStream(t, private, true)
			stream.reads.fail.Store(true)
			stream.service.accessEpoch.move()
			if private {
				typeOver(t, stream.typist, stream.text, stream.text+"x")
				select {
				case <-stream.reader.Ended:
				case <-time.After(roomWait(t)):
					t.Fatalf("control: carol's socket was still open after the rule's read failed and alice typed")
				}
				if code := stream.reader.CloseCode(); code != contracts.DocumentAdmissionRevokedCloseCode {
					t.Errorf("control: carol's socket closed with %d, want %d", code, contracts.DocumentAdmissionRevokedCloseCode)
				}
				if text := peerMarkdown(stream.reader); strings.Contains(text, stream.text+"x") {
					t.Errorf("control: carol's copy holds %q after the rule's read failed", text)
				}
				if stream.reads.failed.Load() == 0 {
					t.Errorf("control: no read failed; the injection did not reach the socket's ask")
				}
				return
			}
			stream.typeFrames(t, 3)
			if failed := stream.reads.failed.Load(); failed == 0 {
				t.Fatalf("UNKNOWN: no read failed after the epoch moved; the row did not test a failed read")
			}
			select {
			case <-stream.reader.Ended:
				t.Fatalf("carol's socket closed (%d) on a failed read while no project is private", stream.reader.CloseCode())
			default:
			}
			stream.reads.fail.Store(false)
			before := stream.reads.private.Load()
			stream.service.accessEpoch.move()
			stream.typeFrames(t, 2)
			if stream.reads.private.Load() == before {
				t.Errorf("no read of whether a project is private after the epoch moved again")
			}
		})
	}
}

// recordingConn is a connection that keeps what is written to it.
type recordingConn struct {
	net.Conn
	mu      sync.Mutex
	written [][]byte
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written = append(c.written, slices.Clone(p))
	return len(p), nil
}

// TestADocumentSocketDropsDataFramesOnceItsContextHasEnded: once a document socket's context has
// ended - a re-check revoked its admission, or its request is gone - its connection drops every
// data frame of the room (socketAsk answers no without a read), and passes a control frame, the
// close frame ygo sends among them.
//
// Control: the same frames on a socket whose context stands pass, data frame included.
func TestADocumentSocketDropsDataFramesOnceItsContextHasEnded(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	upgrade := []byte("HTTP/1.1 101 Switching Protocols\r\n\r\n")
	data := []byte{0x82, 0x02, 0xAA, 0xBB}
	closing := []byte{0x88, 0x02, 0x03, 0xE8}
	for _, ended := range []bool{true, false} {
		t.Run(map[bool]string{true: "ended", false: "control: standing"}[ended], func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			if ended {
				cancel(errAccessRevoked)
			}
			admission := &schemaAdmission{person: "alice", key: schemaAdmissionKey{room: artifactID}}
			conn := &recordingConn{}
			socket := &recheckedConn{Conn: conn, stillReads: service.socketAsk(ctx, admission, model.Viewer{Person: "alice", Open: true})}
			for _, frame := range [][]byte{upgrade, data, closing} {
				if _, err := socket.Write(frame); err != nil {
					t.Fatalf("write a frame: %v", err)
				}
			}
			want := [][]byte{upgrade, data, closing}
			if ended {
				want = [][]byte{upgrade, closing}
			}
			if !slices.EqualFunc(conn.written, want, bytes.Equal) {
				t.Errorf("the connection wrote %x, want %x", conn.written, want)
			}
		})
	}
}
