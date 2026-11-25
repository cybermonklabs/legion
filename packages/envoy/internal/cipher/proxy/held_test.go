// packages/envoy/internal/cipher/proxy/held_test.go
//
// The hold at the airlock: a grant whose enrollment acts for a person Dispatch holds opens no new
// connection and carries no further request on a connection opened before the hold, and the calls
// it already carries - a joined connection, a streamed answer - are cut at the watch's next read,
// while the grants of a person Dispatch does not hold go on. Each row runs a real hold.Gate over a
// Dispatch stand-in (projectaccesstest) and a real broker store, with the clock the gate and its
// Dispatch client read moved by hand past the client's 30-second cache (projectaccess.UseClock).
package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/hold"
	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess/projectaccesstest"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// The two people of the hold's rows: ada, whom Dispatch holds once a row says so, and bob, whom it
// never holds. ada's grant is the rig's own; bob's is a grant of his own of the same name, bound to
// the same host, with a value of its own.
const (
	heldPerson   = "ada@example.com"
	servedPerson = "bob@example.com"
	bobGrantID   = "grant-bob-00000"
	bobEnrollID  = "enrollment-bob-00000"
	bobValue     = "ghp-the-real-bob-token"
)

var bobPlaceholder = placeholder.Prefix + "bobbobbobbobbobbobbobbobbobbob01"

// The words K2 gives the airlock's answers to ada, spelled out here rather than read from the
// proxy, so a change to what the proxy says is a change this row sees. adaDirectoryHeld is what a
// broker that requires a deadline (an IAP deployment's) says of a hold instead, and adaLapsed what
// it says once Dispatch's answer that ada is not held has passed its deadline.
const (
	adaHeld          = "ada@example.com's credentials are held by Dispatch; they work again when ada@example.com signs in to Dispatch, or when Dispatch can confirm ada@example.com with Google again"
	adaDirectoryHeld = "Google's Workspace Directory no longer confirms ada@example.com; their credentials work again once it does"
	adaUncheckable   = "whether Dispatch holds ada@example.com cannot be read: "
	unkeptHeldAnswer = adaUncheckable + "the broker could not keep Dispatch's answer that holds them"
	adaLapsed        = adaUncheckable + "Dispatch's answer that they are not held has passed its deadline, or Dispatch has never confirmed them"
	brokerRefused    = "Dispatch refused the broker's own account"
)

// standInAccount is the bearer the gate's Dispatch client presents: the stand-in's own, or, once a
// row refuses it, one the stand-in refuses, which is Dispatch refusing the broker's own account
// (dispatchread.ErrBrokerRefused).
type standInAccount struct {
	bearer  string
	refused atomic.Bool
}

func (a *standInAccount) Read() (string, error) {
	if a.refused.Load() {
		return "a bearer the stand-in does not take", nil
	}
	return a.bearer, nil
}

// heldStand is the service behind the bound host for these rows: a joined connection and a
// streamed answer for each person, at a path of each one's own, and the stand-in service's own
// line everywhere else.
type heldStand struct {
	joins   map[string]*frameStand
	streams map[string]chan error
}

func (h *heldStand) Answer(w http.ResponseWriter, request *http.Request, seen proxyrulestest.Request) bool {
	if join, ok := h.joins[seen.Path]; ok {
		return join.Answer(w, request, seen)
	}
	ended, ok := h.streams[seen.Path]
	if !ok {
		return false
	}
	w.WriteHeader(http.StatusOK)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, _ = io.WriteString(w, "a line of the stream\n")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-request.Context().Done():
			ended <- request.Context().Err()
			return true
		case <-ticker.C:
		}
	}
}

// heldRig is the rig with a real hold in front of the proxy: the gate's Dispatch stand-in, the
// store the gate keeps its answers in, the clock the gate and its client read, the account the
// client presents, the count of answers, how many connections the proxy port has accepted, and
// the words the gate gives a hold of ada.
type heldRig struct {
	*rig
	dispatch  *projectaccesstest.Server
	store     *store.Store
	clock     *testClock
	account   *standInAccount
	registry  *metrics.Registry
	accepts   atomic.Int64
	stand     *heldStand
	heldWords string
}

// newHeldRig builds the rig with ada's grant and bob's, the proxy counting its answers on answers,
// broker_answers_total on registry, which it makes there when answers is nil: a table that counts
// many rigs' answers on one counter hands its own.
func newHeldRig(t *testing.T, registry *metrics.Registry, answers *metrics.Counter) *heldRig {
	t.Helper()
	return newHeldRigWith(t, registry, answers, false)
}

// newHeldRigWith is newHeldRig with the gate's RequireDeadline set as requireDeadline says, as
// BROKER_HOLD_REQUIRES_DEADLINE sets it on an IAP deployment's broker. Such a broker serves a
// person only on an answer with a deadline, so the stand-in then answers ada not held until a day
// after the rig's clock starts, and bob until two days after, as an IAP deployment's Dispatch
// answers each person with their last active Workspace Directory answer plus 24 hours.
func newHeldRigWith(t *testing.T, registry *metrics.Registry, answers *metrics.Counter, requireDeadline bool) *heldRig {
	t.Helper()
	if answers == nil {
		answers = answer.NewAnswers(registry)
	}
	h := &heldRig{
		store:     storetest.Open(t),
		clock:     &testClock{now: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)},
		registry:  registry,
		stand:     &heldStand{joins: map[string]*frameStand{}, streams: map[string]chan error{}},
		heldWords: adaHeld,
	}
	h.dispatch = projectaccesstest.NewServer(t, nil, map[string]string{heldPerson: projectaccess.MemberFull, servedPerson: projectaccess.MemberFull})
	if requireDeadline {
		h.dispatch.SetNotHeldUntil(heldPerson, h.clock.read().Add(24*time.Hour))
		h.dispatch.SetNotHeldUntil(servedPerson, h.clock.read().Add(48*time.Hour))
		h.heldWords = adaDirectoryHeld
	}
	h.account = &standInAccount{bearer: h.dispatch.Token}
	client := projectaccess.New(h.dispatch.URL, h.account, nil)
	client.UseClock(h.clock.read)
	gate := hold.New(hold.Config{Access: client, Store: h.store, Log: slog.New(slog.DiscardHandler), Now: h.clock.read, RequireDeadline: requireDeadline})
	h.rig = newRig(t, Options{Hold: gate, Watch: watchFast, Answers: answers}, func(c net.Conn) net.Conn {
		h.accepts.Add(1)
		return c
	})

	ada := testConnection()
	ada.Operator = heldPerson
	h.grants.setConnection(workerPlaceholder, ada)
	bob := testConnection()
	bob.GrantID, bob.EnrollmentID, bob.Operator = bobGrantID, bobEnrollID, servedPerson
	bob.Names = map[string]requests.AirlockGrantName{workerName: {Slots: map[string]string{requests.SlotValue: bobPlaceholder}}}
	h.grants.setConnection(bobPlaceholder, bob)
	h.grants.setValues(bobGrantID, storedValues(workerName, bobValue))
	// Every path the stand answers, filled in before it serves anything, so its maps are only
	// read while the service runs.
	for _, who := range []string{"ada", "bob"} {
		h.stand.joins[joinPathOf(who)] = &frameStand{path: joinPathOf(who), done: make(chan struct{})}
		h.stand.streams[streamPathOf(who)] = make(chan error, 1)
	}
	h.answerWith(h.stand.Answer)
	return h
}

// joinPathOf and streamPathOf are the paths the stand answers one person's joined connection and
// streamed answer at.
func joinPathOf(who string) string   { return "/v1/socket/" + who }
func streamPathOf(who string) string { return "/v1/stream/" + who }

// passThirtySeconds moves the clock the gate and its client read past the client's cache, so the
// next check of each person asks the stand-in again.
func (h *heldRig) passThirtySeconds() {
	h.clock.set(h.clock.read().Add(projectaccess.CacheTTL + time.Second))
}

// refuseStandingWrites makes person_standing refuse every insert and update, as a database that
// takes reads and no writes: the gate cannot keep the next held answer Dispatch gives it.
func (h *heldRig) refuseStandingWrites(t *testing.T) {
	t.Helper()
	for _, statement := range []string{
		`create function refuse_person_standing_writes() returns trigger language plpgsql as $$
			begin raise exception 'this row refuses every write to person_standing'; end $$`,
		`create trigger person_standing_refused before insert or update on person_standing
			for each row execute function refuse_person_standing_writes()`,
	} {
		if _, err := h.store.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

// counted is the count of the proxy's answers under outcome and code.
func (h *heldRig) counted(t *testing.T, outcome, code string) int {
	t.Helper()
	return answersCounted(t, h.registry, `outcome="`+outcome+`",code="`+code+`",caller_class="`+answer.CallerAirlock+`"`)
}

// side is one person's calls through the proxy, each opened before any hold: a client whose one
// connection stays open between its requests, a joined connection and a streamed answer.
type side struct {
	who, placeholder, value, grantID string

	open       *http.Client
	join       net.Conn
	joinReader *bufio.Reader
	joinStand  *frameStand
	stream     *bufio.Reader
	streamEnd  chan error
}

// openSide serves each of one person's calls once, as before any hold: a new connection, a request
// on that open connection answered from the grant's values the first one cached, a joined
// connection carrying a frame, and a streamed answer's first line.
func (h *heldRig) openSide(t *testing.T, who, placeholder, value, grantID string) *side {
	t.Helper()
	s := &side{who: who, placeholder: placeholder, value: value, grantID: grantID, open: h.client(placeholder)}
	h.served(t, s.open, s, "a new connection")
	reads, accepts := h.grants.valueReads(grantID), h.accepts.Load()
	h.served(t, s.open, s, "a request on its open connection")
	if got := h.grants.valueReads(grantID); got != reads {
		t.Fatalf("%s's second request read the grant's values %d times more; want it answered from the values its connection's first request cached", who, got-reads)
	}
	if got := h.accepts.Load(); got != accepts {
		t.Fatalf("%s's second request opened %d connections; want it carried on the open one", who, got-accepts)
	}

	s.joinStand, s.streamEnd = h.stand.joins[joinPathOf(who)], h.stand.streams[streamPathOf(who)]
	upgraded, join, joinReader := upgradeThroughPath(t, h.rig, placeholder, http.MethodGet, joinPathOf(who))
	if upgraded.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("%s's upgrade answered %d; want 101", who, upgraded.StatusCode)
	}
	s.join, s.joinReader = join, joinReader
	frame(t, s.join, s.joinReader, who+"'s frame before any hold")
	s.stream = h.streamAs(t, placeholder, streamPathOf(who))
	return s
}

// served makes one request through client and fails t unless the service answered it with the
// person's own value in it.
func (h *heldRig) served(t *testing.T, client *http.Client, s *side, what string) {
	t.Helper()
	seen := len(h.requests())
	got, body := h.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+s.placeholder)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("%s: %s was answered %d %q; want the service's 200", s.who, what, got.StatusCode, body)
	}
	seenNow := h.requests()
	if len(seenNow) != seen+1 {
		t.Fatalf("%s: %s reached the service %d times; want once", s.who, what, len(seenNow)-seen)
	}
	if auth := seenNow[len(seenNow)-1].Header.Get("Authorization"); auth != "Bearer "+s.value {
		t.Fatalf("%s: %s reached the service with Authorization %q; want %s's own value", s.who, what, auth, s.who)
	}
}

// streamAs opens a streamed answer at path through the proxy with placeholder as its proxy
// password, reads its first line, and answers the rest of it, which runs until the row ends or
// the proxy cuts it.
func (h *heldRig) streamAs(t *testing.T, placeholder, path string) *bufio.Reader {
	t.Helper()
	client := h.client(placeholder)
	client.Timeout = 0
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+boundHost+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+placeholder)
	streamed, err := client.Do(request)
	if err != nil {
		t.Fatalf("the streamed call through the proxy: %v", err)
	}
	t.Cleanup(func() { streamed.Body.Close() })
	reader := bufio.NewReader(streamed.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "a line of the stream\n" {
		t.Fatalf("the stream's first line is %q (%v), want the service's", line, err)
	}
	return reader
}

// refusedNewConnection makes a new connection and a plain request as s, and fails t unless each is
// refused with status, code and words, and nothing reaches the service.
func (h *heldRig) refusedNewConnection(t *testing.T, s *side, status int, code, words, when string) {
	t.Helper()
	seen := len(h.requests())
	h.refused(t, status, code, words, s.who+"'s new connection "+when, func() proxyAnswer {
		return connectAnswer(t, h.rig, net.JoinHostPort(boundHost, "443"), basic(s.placeholder))
	})
	h.refused(t, status, code, words, s.who+"'s plain request "+when, func() proxyAnswer {
		return answeredBy(h.plainRaw(tunnelHost, "/", basic(s.placeholder)))
	})
	if got := len(h.requests()) - seen; got != 0 {
		t.Fatalf("%s's new connection and plain request %s reached the service %d times; want no value to leave", s.who, when, got)
	}
}

// refusedOnOpen makes one request on s's connection opened before the hold, and fails t unless it
// is refused with status, code and words on that connection, with nothing reaching the service.
func (h *heldRig) refusedOnOpen(t *testing.T, s *side, status int, code, words, when string) {
	t.Helper()
	seen, accepts := len(h.requests()), h.accepts.Load()
	h.refused(t, status, code, words, s.who+"'s next request on the connection opened before the hold "+when, func() proxyAnswer {
		return answeredBy(h.get(s.open, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+s.placeholder))
	})
	if opened := h.accepts.Load() - accepts; opened != 0 {
		t.Fatalf("%s's next request %s opened %d connections; want it carried on the open one", s.who, when, opened)
	}
	if got := len(h.requests()) - seen; got != 0 {
		t.Fatalf("%s's next request on the open connection %s reached the service %d times; want no value to leave", s.who, when, got)
	}
}

// refused drives one answer and fails t unless it is a refusal of status with code and words: a
// 403's text ends with its answer id, which its one refusal record names with the code, and a 503
// is counted once under its code as a failure, as every answer of this port that refuses nothing
// is. An answer that served the call names the case it was served in.
func (h *heldRig) refused(t *testing.T, status int, code, words, what string, drive func() proxyAnswer) {
	t.Helper()
	failures := h.counted(t, "failed", code)
	got := drive()
	if got.status != status {
		served := ""
		if got.status < 300 {
			served = ": served in " + caseOf(code)
		}
		t.Fatalf("%s was answered %d %q; want %d %s%s", what, got.status, got.body, status, code, served)
	}
	id := got.id()
	if !answerIDPattern.MatchString(id) {
		t.Fatalf("%s carries %s %q, want an answer id", what, answer.Header, id)
	}
	if status == http.StatusForbidden {
		if want := "airlock: " + words + " (answer " + id + ")\n"; got.body != want {
			t.Fatalf("%s was answered %q; want %q", what, got.body, want)
		}
		records := linesNaming(h.logs.refusals(), id)
		if len(records) != 1 || records[0].fields["code"] != code || records[0].fields["reason"] != words {
			t.Fatalf("%s wrote the refusal records %v; want one coded %s with its words", what, records, code)
		}
		return
	}
	if want := "airlock: " + words + "\n"; got.body != want {
		t.Fatalf("%s was answered %q; want %q", what, got.body, want)
	}
	answersCountedReach(t, h.registry, `outcome="failed",code="`+code+`",caller_class="`+answer.CallerAirlock+`"`, failures+1)
}

// caseOf names, for a failure message, the case a row's code is answered in.
func caseOf(code string) string {
	if code == "HOLD_UNCHECKABLE" {
		return "the uncheckable case"
	}
	return "the held case"
}

// heldOrUncheckable names, for a failure message, which case a row's code is: ada held, or the
// uncheckable case.
func heldOrUncheckable(code string) string {
	if code == "HOLD_UNCHECKABLE" {
		return "ada, in the uncheckable case,"
	}
	return "ada, held,"
}

// cutWithinTheWatch fails t unless s's joined connection and streamed answer are each cut by the
// watch, the joined connection and the stream each logging one line with status 403 and the words
// the gate gives a hold of ada, and the service's side of the stream ended.
func (h *heldRig) cutWithinTheWatch(t *testing.T, s *side, when string) {
	t.Helper()
	h.cutWithin(t, s, http.StatusForbidden, h.heldWords, when)
}

// cutWithin fails t unless s's joined connection and streamed answer are each cut by the watch,
// the joined connection and the stream each logging one line with status and reason, and the
// service's side of the stream ended.
func (h *heldRig) cutWithin(t *testing.T, s *side, status int, reason, when string) {
	t.Helper()
	if err := s.join.SetReadDeadline(time.Now().Add(cutWait)); err != nil {
		t.Fatalf("set the read deadline: %v", err)
	}
	if rest, err := io.ReadAll(s.joinReader); err != nil || len(rest) != 0 {
		t.Fatalf("%s's joined connection %s was not cut within %v, the watch interval being %v (%q, %v): ada was served", s.who, when, cutWait, watchFast, rest, err)
	}
	_, _ = s.join.Write([]byte("a frame after the hold\n"))
	time.Sleep(5 * watchFast)
	if frames := s.joinStand.read(); !slices.Equal(frames, []string{s.who + "'s frame before any hold"}) {
		t.Fatalf("the service read %q on %s's joined connection; want the one frame from before the hold", frames, s.who)
	}

	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, s.stream)
		copied <- err
	}()
	select {
	case err := <-copied:
		if err == nil {
			t.Fatalf("%s's streamed answer %s ended cleanly; want it cut", s.who, when)
		}
	case <-time.After(cutWait):
		t.Fatalf("%s's streamed answer %s was not cut within %v, the watch interval being %v: ada was served", s.who, when, cutWait, watchFast)
	}
	select {
	case err := <-s.streamEnd:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the service's side of %s's stream ended with %v, want it cancelled", s.who, err)
		}
	case <-time.After(cutWait):
		t.Fatalf("the service went on streaming to %s after the proxy cut the command's side", s.who)
	}

	for _, lines := range [][]logLine{h.logs.joins(), h.logs.cuts()} {
		naming := 0
		for _, line := range lines {
			if line.fields["grant"] != s.grantID {
				continue
			}
			naming++
			if line.fields["reason"] != reason || line.fields["status"] != int64(status) {
				t.Errorf("the cut of %s's call logged status %v and reason %v; want %d and %q", s.who, line.fields["status"], line.fields["reason"], status, reason)
			}
		}
		if naming != 1 {
			t.Fatalf("the proxy logged %d lines for the cut of %s's call; want one: %v", naming, s.who, lines)
		}
	}
}

// carriesOn fails t unless s's joined connection still carries a frame and the stream still
// streams, with no line of a cut naming s's grant.
func (h *heldRig) carriesOn(t *testing.T, s *side, when string) {
	t.Helper()
	frame(t, s.join, s.joinReader, s.who+"'s frame "+when)
	select {
	case err := <-s.streamEnd:
		t.Fatalf("the service's side of %s's stream ended %s (%v); want it carried on", s.who, when, err)
	default:
	}
	if line, err := s.stream.ReadString('\n'); err != nil || line != "a line of the stream\n" {
		t.Fatalf("%s's stream %s read %q (%v); want it carried on", s.who, when, line, err)
	}
	for _, line := range append(h.logs.joins(), h.logs.cuts()...) {
		if line.fields["grant"] == s.grantID {
			t.Fatalf("the proxy cut %s's call %s: %v", s.who, when, line.fields)
		}
	}
}

// TestHeldAirlockOpenConnectionIsRefused: for ada, a new connection, a request on an open bound
// connection with the grant's values cached, a joined connection and a streamed answer are each
// served; the stand-in then answers ada held and the gate's clock moves past 30 seconds. A new
// connection and a plain request are then refused 403 MEMBERSHIP_HELD, as is the next request on
// the open connection, with no value leaving and none read; and the joined connection and the
// streamed answer are cut within the watch. When the broker cannot keep the held answer, the new
// connection and the next request are refused 503 HOLD_UNCHECKABLE and the carried calls are cut
// all the same. With the stand-in refusing the broker's account, a new request is refused 503
// HOLD_UNCHECKABLE, and a carried call is left alone. Bob's are served throughout the hold.
//
// The named reverts: take the gate's check out of serveProxy, serveBound or carryRefusal, and the
// row of that path fails naming ada served; read hold.ErrUncheckable as nil in holdRefusal, and
// the rows of the uncheckable case fail naming it.
func TestHeldAirlockOpenConnectionIsRefused(t *testing.T) {
	for _, row := range []struct {
		name      string
		unkept    bool
		status    int
		code      string
		words     string
		refusedTo string
	}{
		{name: "Dispatch holds ada", status: http.StatusForbidden, code: "MEMBERSHIP_HELD", words: adaHeld, refusedTo: "after the hold"},
		{name: "Dispatch holds ada and the broker cannot keep that answer", unkept: true,
			status: http.StatusServiceUnavailable, code: "HOLD_UNCHECKABLE", words: unkeptHeldAnswer, refusedTo: "after the unkept hold"},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newHeldRig(t, metrics.New(), nil)
			ada := h.openSide(t, "ada", workerPlaceholder, workerValue, testGrantID)
			bob := h.openSide(t, "bob", bobPlaceholder, bobValue, bobGrantID)

			h.dispatch.SetHeld(heldPerson, true)
			if row.unkept {
				h.refuseStandingWrites(t)
			}
			h.passThirtySeconds()

			h.cutWithinTheWatch(t, ada, row.refusedTo)
			h.refusedNewConnection(t, ada, row.status, row.code, row.words, row.refusedTo)
			// The policy version moves first, so the values this pod holds for ada's grant are
			// stale and any read of them would reach the store: the hold is asked before it.
			h.grants.setPolicyVersion("policy-after-the-hold")
			reads := h.grants.valueReads(testGrantID)
			h.refusedOnOpen(t, ada, row.status, row.code, row.words, row.refusedTo)
			if got := h.grants.valueReads(testGrantID) - reads; got != 0 {
				t.Fatalf("ada's next request on the open connection %s read the grant's values %d times; want the hold asked before any value is read", row.refusedTo, got)
			}

			time.Sleep(5 * watchFast)
			h.served(t, h.client(bobPlaceholder), bob, "a new connection "+row.refusedTo)
			h.served(t, bob.open, bob, "the next request on his open connection "+row.refusedTo)
			h.carriesOn(t, bob, row.refusedTo)
		})
	}

	t.Run("Dispatch refuses the broker's own account", func(t *testing.T) {
		h := newHeldRig(t, metrics.New(), nil)
		ada := h.openSide(t, "ada", workerPlaceholder, workerValue, testGrantID)
		bob := h.openSide(t, "bob", bobPlaceholder, bobValue, bobGrantID)

		h.account.refused.Store(true)
		h.passThirtySeconds()
		// Nobody is decided while Dispatch refuses the broker's own account, bob no more than
		// ada: every new request is refused, and every call already carried is left alone.
		for _, s := range []*side{ada, bob} {
			words := "whether Dispatch holds " + s.who + "@example.com cannot be read: " + brokerRefused
			h.refusedNewConnection(t, s, http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", words, "while Dispatch refuses the broker")
			h.refusedOnOpen(t, s, http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", words, "while Dispatch refuses the broker")
		}
		// Three asks of the stand-in, every one refused, so what the carried calls show is what
		// the watch did with an answer that decides nothing rather than a watch that has not run.
		before := h.dispatch.Calls()
		deadline := time.Now().Add(10 * time.Second)
		for h.dispatch.Calls() < before+3 {
			if time.Now().After(deadline) {
				t.Fatalf("the stand-in was asked %d times in ten seconds; want three more than the %d before", h.dispatch.Calls(), before)
			}
			time.Sleep(watchFast)
		}
		h.carriesOn(t, ada, "while Dispatch refuses the broker: the uncheckable case leaves a carried call alone")
		h.carriesOn(t, bob, "while Dispatch refuses the broker")
	})
}

// TestHeldAirlockCarriedCallUnderADeadline: on a broker that requires a deadline
// (BROKER_HOLD_REQUIRES_DEADLINE, an IAP deployment's), with Dispatch answering ada not held until
// a day ahead and bob until two days ahead, each of ada's calls is served before anything changes.
// When Dispatch then holds ada, her joined connection and streamed answer are cut within the watch,
// and a new connection and a plain request are refused, 403 MEMBERSHIP_HELD in the words such a
// broker gives a hold, that the Workspace Directory, not a sign-in, ends it. When instead the gate's
// clock passes ada's deadline with Dispatch still answering it, her carried calls are cut 503
// HOLD_UNCHECKABLE, saying Dispatch's answer that she is not held has passed its deadline, and so
// are her new connection and plain request: a call already carried does not outlive the deadline
// a new one is refused at. Bob's calls are served throughout.
//
// Controls: carryRefusal ending a held person's call in the words of a broker that requires no
// deadline (the held row fails naming the cut's words); carryRefusal leaving a call alone once its
// person's deadline passed (the lapsed row fails naming ada served).
func TestHeldAirlockCarriedCallUnderADeadline(t *testing.T) {
	t.Run("Dispatch holds ada", func(t *testing.T) {
		h := newHeldRigWith(t, metrics.New(), nil, true)
		ada := h.openSide(t, "ada", workerPlaceholder, workerValue, testGrantID)
		bob := h.openSide(t, "bob", bobPlaceholder, bobValue, bobGrantID)

		h.dispatch.SetHeld(heldPerson, true)
		h.passThirtySeconds()
		h.cutWithinTheWatch(t, ada, "after the hold")
		h.refusedNewConnection(t, ada, http.StatusForbidden, "MEMBERSHIP_HELD", adaDirectoryHeld, "after the hold")

		time.Sleep(5 * watchFast)
		h.served(t, h.client(bobPlaceholder), bob, "a new connection after the hold")
		h.carriesOn(t, bob, "after the hold")
	})

	t.Run("ada's deadline passes", func(t *testing.T) {
		h := newHeldRigWith(t, metrics.New(), nil, true)
		ada := h.openSide(t, "ada", workerPlaceholder, workerValue, testGrantID)
		bob := h.openSide(t, "bob", bobPlaceholder, bobValue, bobGrantID)

		h.clock.set(h.clock.read().Add(24*time.Hour + time.Second))
		h.cutWithin(t, ada, http.StatusServiceUnavailable, adaLapsed, "once her deadline passed")
		h.refusedNewConnection(t, ada, http.StatusServiceUnavailable, "HOLD_UNCHECKABLE", adaLapsed, "once her deadline passed")

		time.Sleep(5 * watchFast)
		h.served(t, h.client(bobPlaceholder), bob, "a new connection once ada's deadline passed")
		h.carriesOn(t, bob, "once ada's deadline passed")
	})
}

// dispatchDown is the outage of TestHeldAirlockCarriedCallWhileDispatchIsDown: the gate's Dispatch
// stand-in answers nothing, and the connection read of each grant fails as the requests package's
// project check fails while it cannot ask Dispatch (requests.ErrProjectAccessUnavailable), which
// refuses no grant and leaves a carried call to the reads after it.
func (h *heldRig) dispatchDown() {
	h.dispatch.SetDown(true)
	unavailable := fmt.Errorf("airlock connection: the project check: %w", requests.ErrProjectAccessUnavailable)
	h.grants.refuseConnection(workerPlaceholder, unavailable)
	h.grants.refuseConnection(bobPlaceholder, unavailable)
}

// TestHeldAirlockCarriedCallWhileDispatchIsDown: the hold is asked of a carried call's person even
// when the connection read cannot say whether the grant still carries it, as it cannot while
// Dispatch is down and the grant holds a project-tagged name. Each of ada's and bob's calls is
// served; then Dispatch goes down, and with it the project check of each connection read.
//
//   - A hold the broker kept: another broker process kept Dispatch's answer that holds ada just
//     before the outage (person_standing), and the gate's 30-second cache of her earlier answer
//     lapses. Her joined connection and streamed answer are cut within the watch, 403
//     MEMBERSHIP_HELD in the gate's words: an outage releases nobody Dispatch held.
//   - A deadline passed: on a broker that requires a deadline, the gate's clock passes ada's kept
//     deadline during the outage. Her carried calls are cut 503 HOLD_UNCHECKABLE, saying Dispatch's
//     answer that she is not held has passed its deadline.
//
// In each, bob, whom nothing holds and whose deadline has not passed, keeps his calls: a project
// check that cannot ask Dispatch ends no carried call.
//
// Control: carryRefusal leaving the call to the connection read when that read fails (the hold
// asked only after a read that succeeded), as it did: ada's calls are carried on, and each row
// fails naming her served.
func TestHeldAirlockCarriedCallWhileDispatchIsDown(t *testing.T) {
	t.Run("a hold the broker kept", func(t *testing.T) {
		h := newHeldRig(t, metrics.New(), nil)
		ada := h.openSide(t, "ada", workerPlaceholder, workerValue, testGrantID)
		bob := h.openSide(t, "bob", bobPlaceholder, bobValue, bobGrantID)

		h.dispatchDown()
		if err := h.store.KeepPersonStanding(context.Background(), heldPerson, true, time.Now(), nil); err != nil {
			t.Fatalf("keep a held answer of %s: %v", heldPerson, err)
		}
		h.passThirtySeconds()
		h.cutWithinTheWatch(t, ada, "while Dispatch is down, the broker having kept her hold")

		time.Sleep(5 * watchFast)
		h.carriesOn(t, bob, "while Dispatch is down")
	})

	t.Run("a deadline passed", func(t *testing.T) {
		h := newHeldRigWith(t, metrics.New(), nil, true)
		ada := h.openSide(t, "ada", workerPlaceholder, workerValue, testGrantID)
		bob := h.openSide(t, "bob", bobPlaceholder, bobValue, bobGrantID)

		h.dispatchDown()
		h.clock.set(h.clock.read().Add(24*time.Hour + time.Second))
		h.cutWithin(t, ada, http.StatusServiceUnavailable, adaLapsed, "while Dispatch is down, once her deadline passed")

		time.Sleep(5 * watchFast)
		h.carriesOn(t, bob, "while Dispatch is down, his deadline a day ahead")
	})
}
