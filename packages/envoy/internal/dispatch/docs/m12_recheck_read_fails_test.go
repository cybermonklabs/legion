package docs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// recheckReadFailedLog is what the access re-check logs, at WARN, for each person whose access it
// could not read while its own context stood (recheckAccess).
const recheckReadFailedLog = "their sockets close"

// failReadOf marks the context of an access re-check whose reads of one person's access fail: its
// value is that person's login (readFailer).
type failReadOf struct{}

// readFailer is a pgx.QueryTracer that fails each statement a context marked failReadOf starts
// whose arguments name that person, as a database that cannot answer the read would: it hands pgx
// a context already ended, so the statement is never sent, while the re-check's own context
// stands. Every other statement runs as it would.
type readFailer struct{}

func (readFailer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	person, _ := ctx.Value(failReadOf{}).(string)
	if person == "" {
		return ctx
	}
	for _, arg := range data.Args {
		if named, ok := arg.(*string); ok && named != nil && *named == person {
			failed, cancel := context.WithCancelCause(ctx)
			cancel(errors.New("the database cannot read " + person + "'s access"))
			return failed
		}
	}
	return ctx
}

func (readFailer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// m12Sockets is a document service on a database readFailer traces, its document's room, and one
// open socket each of alice, DOC's owner, and bob: on DOC's list when DOC is private, a full member
// of a public DOC otherwise, with each socket's admission, which keeps whether a re-check revoked
// it after its ServeHTTP has forgotten it. No room sync runs, so the re-check a row calls is the
// only one.
type m12Sockets struct {
	service    *Service
	room       string
	peers      map[string]*docstest.Peer
	admissions map[string]*schemaAdmission
}

const m12Bob = "bob@outside.example"

func openM12Sockets(t *testing.T, private bool) m12Sockets {
	t.Helper()
	database := storetest.TracedStore(t, storetest.Open(t), readFailer{})
	room := createDocument(t, database, "# First")
	service := newTestServiceOn(t, database)
	seedServiceText(t, service, room, "before")
	owner := model.Actor{Kind: "user", ID: "alice"}
	if private {
		writeAccess(t, service, func(ctx context.Context, tx pgx.Tx) error {
			if _, _, err := access.SetVisibility(ctx, tx, service.events, "DOC", model.ProjectPrivate, owner); err != nil {
				return err
			}
			_, _, err := access.AddPrincipal(ctx, tx, service.events, "DOC", access.PrincipalPerson, m12Bob, owner)
			return err
		})
	}
	var anyPrivate bool
	if err := service.store.Pool.QueryRow(context.Background(),
		`select exists (select 1 from projects where visibility = 'private')`).Scan(&anyPrivate); err != nil {
		t.Fatalf("read whether any project is private: %v", err)
	}
	if anyPrivate != private {
		t.Fatalf("a project is private: %t; the row needs %t", anyPrivate, private)
	}
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	testcleanup.Add(t, httpServer.Close)
	peers := make(map[string]*docstest.Peer)
	for _, login := range []string{"alice", m12Bob} {
		peer := connectPeer(t, httpServer.URL, room, login)
		if err := peer.WaitForText(peerMarkdown, "before\n", roomWait(t)); err != nil {
			t.Fatalf("%s reads the document of DOC: %v", login, err)
		}
		peers[login] = peer
	}
	held := heldSocketsByPerson(service)
	admissions := make(map[string]*schemaAdmission)
	for _, login := range []string{"alice", m12Bob} {
		if len(held[login]) != 1 || socketRevoked(service, held[login][0]) {
			t.Fatalf("before the re-check the service holds %d socket(s) of %s; want one open socket each of alice and bob", len(held[login]), login)
		}
		admissions[login] = held[login][0]
	}
	return m12Sockets{service: service, room: room, peers: peers, admissions: admissions}
}

// heldSocketsByPerson is every document socket service holds, by its person.
func heldSocketsByPerson(service *Service) map[string][]*schemaAdmission {
	admissions := &service.schemaAdmissions
	admissions.mu.Lock()
	defer admissions.mu.Unlock()
	held := make(map[string][]*schemaAdmission)
	for admission := range admissions.sockets {
		held[admission.person] = append(held[admission.person], admission)
	}
	return held
}

// socketRevoked is whether a re-check has closed admission's socket on service.
func socketRevoked(service *Service, admission *schemaAdmission) bool {
	service.schemaAdmissions.mu.Lock()
	defer service.schemaAdmissions.mu.Unlock()
	return admission.revoked
}

// requireOutcome checks what one access re-check left: each login of closed has its socket
// revoked, and that socket ends with the close code on which the provider reconnects; each login of
// kept has its socket not revoked, and still open a second after the closes, which one pass makes
// all at once.
func (s m12Sockets) requireOutcome(t *testing.T, closed, kept []string) {
	t.Helper()
	var ending []string
	for _, login := range closed {
		if !socketRevoked(s.service, s.admissions[login]) {
			t.Errorf("the re-check left %s's document socket open; want it closed, as a re-check that cannot read a person's access closes their sockets", login)
			continue
		}
		ending = append(ending, login)
	}
	for _, login := range kept {
		if socketRevoked(s.service, s.admissions[login]) {
			t.Errorf("the re-check closed %s's document socket, though %s still reads DOC and that read answered", login, login)
		}
	}
	for _, login := range ending {
		peer := s.peers[login]
		select {
		case <-peer.Ended:
		case <-time.After(roomWait(t)):
			t.Fatalf("%s's revoked document socket was still open %s after the re-check", login, roomWait(t))
		}
		if code := peer.CloseCode(); code != contracts.DocumentAdmissionRevokedCloseCode {
			t.Errorf("%s's document socket closed with %d, want %d, on which the provider reconnects", login, code, contracts.DocumentAdmissionRevokedCloseCode)
		}
	}
	if len(kept) == 0 {
		return
	}
	<-time.After(time.Second)
	for _, login := range kept {
		select {
		case <-s.peers[login].Ended:
			t.Errorf("%s's document socket closed (%d), though %s still reads DOC", login, s.peers[login].CloseCode(), login)
		default:
		}
	}
}

// TestAnAccessRecheckWhoseReadFailsClosesTheSocketsItCouldNotReadAgain: the access re-check closes
// a socket whose person it can no longer show reads the room, and a read that fails shows nothing,
// so it closes that person's sockets too (fail closed), with the code on which the provider
// reconnects and whose reconnect's admission reads the person again, as recheckAdmission closes
// a socket whose read fails. A person whose read answered and who still reads the room keeps their
// socket. Each failure: the re-check's own context has ended, which logs nothing, as the process is
// ending; the database refuses every read, here because a relation the read names is renamed in
// the test's own database; and the database refuses bob's read alone. A failure while the
// re-check's context stands is logged at WARN once for each person whose sockets it closes.
func TestAnAccessRecheckWhoseReadFailsClosesTheSocketsItCouldNotReadAgain(t *testing.T) {
	// Not parallel: each row replaces the process's default logger (captureWarnings,
	// logtest.SetDefault) to count the WARN lines the re-check writes.
	for _, row := range []struct {
		name string
		// fail breaks the re-check's read and answers the context the re-check runs on, and what
		// mends the read again.
		fail         func(t *testing.T, service *Service) (ctx context.Context, mend func())
		closed, kept []string
		logged       int
	}{
		{
			name: "the re-check's context has ended",
			fail: func(*testing.T, *Service) (context.Context, func()) {
				ended, cancel := context.WithCancel(context.Background())
				cancel()
				return ended, func() {}
			},
			closed: []string{"alice", m12Bob},
		},
		{
			name: "the database refuses every person's read",
			fail: func(t *testing.T, service *Service) (context.Context, func()) {
				rename := func(from, to string) {
					if _, err := service.store.Pool.Exec(context.Background(), `alter table `+from+` rename to `+to); err != nil {
						t.Fatalf("rename %s to %s: %v", from, to, err)
					}
				}
				rename("project_access", "project_access_unread")
				mended := false
				mend := func() {
					if !mended {
						mended = true
						rename("project_access_unread", "project_access")
					}
				}
				testcleanup.Add(t, mend)
				return context.Background(), mend
			},
			closed: []string{"alice", m12Bob},
			logged: 2,
		},
		{
			name: "the database refuses bob's read alone",
			fail: func(*testing.T, *Service) (context.Context, func()) {
				return context.WithValue(context.Background(), failReadOf{}, m12Bob), func() {}
			},
			closed: []string{m12Bob},
			kept:   []string{"alice"},
			logged: 1,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			logs := captureWarnings(t)
			sockets := openM12Sockets(t, true)
			ctx, mend := row.fail(t, sockets.service)
			sockets.service.recheckAccess(ctx)
			// The break is what the row names: each read of a login of closed fails on ctx, and each
			// of kept answers, so a socket left open is the re-check's and not an unbroken read's.
			probe := []*schemaAdmission{{key: schemaAdmissionKey{room: sockets.room}}}
			for _, login := range row.closed {
				if _, err := sockets.service.readableRooms(ctx, login, probe); err == nil {
					t.Fatalf("%s's read of DOC answered on the row's broken read; the row breaks nothing", login)
				}
			}
			for _, login := range row.kept {
				if _, err := sockets.service.readableRooms(ctx, login, probe); err != nil {
					t.Fatalf("%s's read of DOC failed on the row's broken read (%v); want it to answer", login, err)
				}
			}
			mend()
			sockets.requireOutcome(t, row.closed, row.kept)
			if lines := logs.Lines(recheckReadFailedLog); strings.Count(lines, "level=WARN") != row.logged || strings.Count(lines, "\n") != row.logged {
				t.Errorf("the re-check logged %q; want %d WARN line(s) naming the sockets it closes for a read that failed", lines, row.logged)
			}
		})
	}
}

// TestAnAccessRecheckWhoseReadAnswersKeepsEverySocketWhosePersonStillReadsTheRoom: with each read
// answering, a re-check closes no socket whose person still reads its room and logs nothing: in a
// deployment where no project is private, as before the re-check closed anything, and with DOC
// private and bob on its list.
func TestAnAccessRecheckWhoseReadAnswersKeepsEverySocketWhosePersonStillReadsTheRoom(t *testing.T) {
	// Not parallel: each row replaces the process's default logger (captureWarnings,
	// logtest.SetDefault) to read that the re-check writes nothing.
	for _, row := range []struct {
		name    string
		private bool
	}{
		{name: "no project is private", private: false},
		{name: "DOC is private and lists bob", private: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			logs := captureWarnings(t)
			sockets := openM12Sockets(t, row.private)
			sockets.service.recheckAccess(context.Background())
			sockets.requireOutcome(t, nil, []string{"alice", m12Bob})
			if written := logs.String(); strings.Contains(written, "document socket") {
				t.Errorf("a re-check whose every read answered logged %q; want nothing of the document sockets", written)
			}
		})
	}
}
