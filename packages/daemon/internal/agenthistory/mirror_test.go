package agenthistory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const listenerToken = "envoy-bearer"

// fakeClock hands every wait the mirror asks for to the test, which fires it.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan fakeWait
}

type fakeWait struct {
	d    time.Duration
	fire chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), waits: make(chan fakeWait, 64)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	w := fakeWait{d: d, fire: make(chan time.Time, 1)}
	c.waits <- w
	return w.fire
}

// wait is the next wait the mirror asks for, once its pass is over.
func (c *fakeClock) wait(t *testing.T) fakeWait {
	t.Helper()
	select {
	case w := <-c.waits:
		return w
	case <-time.After(10 * time.Second):
		t.Fatal("the mirror asked for no wait")
		return fakeWait{}
	}
}

// fire moves time to the wait's end and ends it.
func (c *fakeClock) fire(w fakeWait) {
	c.mu.Lock()
	c.now = c.now.Add(w.d)
	now := c.now
	c.mu.Unlock()
	w.fire <- now
}

// fakeStore holds the history in memory, with the standing the test gives each of its issues.
type fakeStore struct {
	mu   sync.Mutex
	rows []store.AgentConversation
	// finished is what FinishedIssues says of each issue; an issue it does not name is running.
	finished map[string]bool
	// standingErr is what FinishedIssues fails with while it is set.
	standingErr error
}

// setFinished is the record moving an issue out of the workflow (a close, a park, its tree
// retired) or back into it.
func (s *fakeStore) setFinished(issue string, finished bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished == nil {
		s.finished = map[string]bool{}
	}
	s.finished[issue] = finished
}

func (s *fakeStore) failStandings(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.standingErr = err
}

func (s *fakeStore) RegisterConversation(_ context.Context, c store.AgentConversation) (store.AgentConversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.Source = store.ConversationRegistered
	s.rows = append(s.rows, c)
	return c, nil
}

func (s *fakeStore) ScanConversation(context.Context, store.AgentConversation) (bool, error) {
	return false, fmt.Errorf("the mirror scans nothing")
}

func (s *fakeStore) AgentConversations(_ context.Context, project string) ([]store.AgentConversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []store.AgentConversation
	for _, row := range s.rows {
		if row.Project == project {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// IssueConversations is every row of one issue of the project (store.IssueConversations).
func (s *fakeStore) IssueConversations(_ context.Context, project, issue string) ([]store.AgentConversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []store.AgentConversation
	for _, row := range s.rows {
		if row.Project == project && row.Issue == issue {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// FinishedIssues names every issue the project's rows carry, with the standing the test set it,
// and checks that the mirror reads the record's own rule (store.FinishedIssues takes
// record.OutOfWorkflow, which no other function answers the same way).
func (s *fakeStore) FinishedIssues(_ context.Context, project string, outOfWorkflow func(string) bool) (map[string]bool, error) {
	if outOfWorkflow == nil || !outOfWorkflow("done") || !outOfWorkflow("backlog") || outOfWorkflow("in_progress") {
		return nil, fmt.Errorf("the mirror passed a rule that is not record.OutOfWorkflow")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.standingErr != nil {
		return nil, s.standingErr
	}
	finished := map[string]bool{}
	for _, row := range s.rows {
		if row.Project == project {
			finished[row.Issue] = s.finished[row.Issue]
		}
	}
	return finished, nil
}

// beat is one heartbeat the stand-in took.
type beat struct {
	Answering bool   `json:"answering"`
	UpdatedAt string `json:"updated_at"`
}

// standIn is the Envoy listener's Legion agent registry as dispatch://INFRA-448's contract and
// INFRA-204's plan 2.1 give it: PUT /v1/legion/agents/sessions/{id} with one full entry whose
// session_id is the path's, PUT /v1/legion/agents with at most 256 entries in at most 1 MiB, each
// validated before any is written (400 naming the index), a re-put changing a session's binding
// refused 409, and PUT /v1/legion/projects/{project} {answering, updated_at}; every route behind the
// listener's bearer. status answers a route in place of all that (0 for none).
type standIn struct {
	t      *testing.T
	mu     sync.Mutex
	status map[string]int
	// conflict is a session whose stored binding every put of it changes.
	conflict string

	batches [][]Entry
	puts    []Entry
	beats   []beat
	stored  map[string]Entry
}

func newStandIn(t *testing.T) (*standIn, *httptest.Server) {
	s := &standIn{t: t, status: map[string]int{}, stored: map[string]Entry{}}
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(server.Close)
	return s, server
}

func (s *standIn) setStatus(route string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[route] = status
}

func (s *standIn) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+listenerToken {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"read"}`, http.StatusBadRequest)
		return
	}
	route := ""
	switch {
	case r.URL.Path == "/v1/legion/agents":
		route = "batch"
	case strings.HasPrefix(r.URL.Path, "/v1/legion/agents/sessions/"):
		route = "session"
	case strings.HasPrefix(r.URL.Path, "/v1/legion/projects/"):
		route = "project"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if status := s.status[route]; status != 0 || route == "" || r.Method != http.MethodPut {
		if status == 0 {
			status = http.StatusNotFound
		}
		http.Error(w, `{"error":"not found"}`, status)
		return
	}
	switch route {
	case "batch":
		if len(body) > batchBytes {
			s.t.Errorf("a batch of %d bytes, over the route's 1 MiB", len(body))
			http.Error(w, `{"error":"too large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		var batch struct {
			Entries []json.RawMessage `json:"entries"`
		}
		if err := strictly(body, &batch); err != nil || len(batch.Entries) == 0 || len(batch.Entries) > batchEntries {
			s.t.Errorf("a batch the route refuses (%d entries): %v", len(batch.Entries), err)
			http.Error(w, `{"error":"bad batch"}`, http.StatusBadRequest)
			return
		}
		var entries []Entry
		for i, raw := range batch.Entries {
			entry, err := s.entry(raw)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"entry %d: %v"}`, i, err), http.StatusBadRequest)
				return
			}
			if entry.SessionID == s.conflict {
				http.Error(w, `{"error":"legion agent `+entry.SessionID+` is recorded for another issue"}`, http.StatusConflict)
				return
			}
			entries = append(entries, entry)
		}
		for _, entry := range entries {
			s.stored[entry.SessionID] = entry
		}
		s.batches = append(s.batches, entries)
	case "session":
		entry, err := s.entry(body)
		if err != nil || "/v1/legion/agents/sessions/"+entry.SessionID != r.URL.Path {
			s.t.Errorf("a session put the route refuses (%s): %v", r.URL.Path, err)
			http.Error(w, `{"error":"bad entry"}`, http.StatusBadRequest)
			return
		}
		if entry.SessionID == s.conflict {
			http.Error(w, `{"error":"legion agent `+entry.SessionID+` is recorded for another issue"}`, http.StatusConflict)
			return
		}
		s.stored[entry.SessionID] = entry
		s.puts = append(s.puts, entry)
	case "project":
		var b beat
		if err := strictly(body, &b); err != nil || r.URL.Path != "/v1/legion/projects/"+legionProject {
			s.t.Errorf("a heartbeat the route refuses (%s): %v", r.URL.Path, err)
			http.Error(w, `{"error":"bad heartbeat"}`, http.StatusBadRequest)
			return
		}
		s.beats = append(s.beats, b)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

// entry is raw as the contract's entry: exactly its eight fields, the role one of the six, the
// token the one claim.NewToken builds from legion_project, issue and role, the time RFC 3339 UTC.
func (s *standIn) entry(raw []byte) (Entry, error) {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Entry{}, err
	}
	want := []string{"harness", "issue", "legion_project", "role", "role_token", "session_id", "started_at", "title"}
	got := make([]string, 0, len(fields))
	for key := range fields {
		// finished is the one optional field: it is written only for an issue that has left the
		// workflow, and omitted while the issue runs, which is what the listener reads as false.
		if key == "finished" {
			if value, ok := fields[key].(bool); !ok || !value {
				return Entry{}, fmt.Errorf("finished is %v, want it written only when it is true", fields[key])
			}
			continue
		}
		got = append(got, key)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		return Entry{}, fmt.Errorf("fields %v, want %v", got, want)
	}
	var entry Entry
	if err := strictly(raw, &entry); err != nil {
		return Entry{}, err
	}
	token, err := claim.NewToken(entry.LegionProject, entry.Issue, claim.Role(entry.Role))
	if err != nil || string(token) != entry.RoleToken {
		return Entry{}, fmt.Errorf("role_token %s is not the one %s, %s and %s build", entry.RoleToken, entry.LegionProject, entry.Issue, entry.Role)
	}
	if at, err := time.Parse(time.RFC3339, entry.StartedAt); err != nil || at.Location() != time.UTC || !strings.HasSuffix(entry.StartedAt, "Z") {
		return Entry{}, fmt.Errorf("started_at %q is not RFC 3339 UTC", entry.StartedAt)
	}
	if entry.Harness != "omp" && entry.Harness != "claude" {
		return Entry{}, fmt.Errorf("harness %q", entry.Harness)
	}
	return entry, nil
}

func strictly(raw []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

func (s *standIn) snapshot() (batches [][]Entry, puts []Entry, beats []beat) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.batches), slices.Clone(s.puts), slices.Clone(s.beats)
}

// testerRow is a registered conversation of LEGION-7's tester (in the Dispatch project LEGION,
// under the Legion project token sjawharlegion), started minute minutes after the clock's start.
func testerRow(t *testing.T, session string, minute int) store.AgentConversation {
	t.Helper()
	return store.AgentConversation{
		Session: session, Project: legionProject, Issue: "LEGION-7", Role: claim.RoleTester, Token: token(t, "LEGION-7", claim.RoleTester),
		Harness: runtime.BackendOmp, Location: "/sessions/" + session + ".jsonl",
		StartedAt: time.Date(2026, 10, 20, 9, minute, 0, 0, time.UTC), Source: store.ConversationRegistered,
	}
}

// runMirror runs the mirror of a daemon whose answer loop runs over st against the stand-in until
// the test ends.
func runMirror(t *testing.T, server *httptest.Server, st Store, log *logs) (*Mirror, *fakeClock) {
	t.Helper()
	return runMirrorOf(t, server, MirrorConfig{Answering: true, Store: st, Log: log.logger()})
}

// runMirrorOf runs a mirror of cfg, with the stand-in's address and bearer, the Legion project and
// a fake clock, against the stand-in until the test ends.
func runMirrorOf(t *testing.T, server *httptest.Server, cfg MirrorConfig) (*Mirror, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	cfg.BaseURL, cfg.Token, cfg.Project, cfg.Clock = server.URL+"/", listenerToken, legionProject, clock
	m := NewMirror(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return m, clock
}

// The boot writes the whole history in batches of at most 256 entries and 1 MiB, oldest first,
// each entry exactly the contract's fields with the Legion project token as legion_project, never
// the Dispatch project its issue keys carry. A scanned conversation that would read as its role's
// newest over a registered one is left out; an older one is written.
func TestTheBootWritesTheHistoryInBatchesWithinTheRoutesBounds(t *testing.T) {
	listener, server := newStandIn(t)
	st := &fakeStore{}
	for i := range 600 {
		st.rows = append(st.rows, testerRow(t, fmt.Sprintf("ses-%04d", i), 0))
	}
	// Session ids long enough that 256 entries would pass 1 MiB.
	long := strings.Repeat("x", 8000)
	for i := range 300 {
		st.rows = append(st.rows, testerRow(t, fmt.Sprintf("long-%04d-%s", i, long), 1))
	}
	older := testerRow(t, "scanned-older", 2)
	older.StartedAt, older.Source = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), store.ConversationScanned
	newer := testerRow(t, "scanned-newer", 59)
	newer.Source = store.ConversationScanned
	st.rows = append(st.rows, older, newer)

	_, clock := runMirror(t, server, st, &logs{})
	clock.wait(t)

	batches, _, _ := listener.snapshot()
	var written []string
	for i, batch := range batches {
		if len(batch) > batchEntries {
			t.Errorf("batch %d holds %d entries, over 256", i, len(batch))
		}
		for _, entry := range batch {
			written = append(written, entry.SessionID)
		}
	}
	if len(batches) < 4 || len(batches[0]) != batchEntries || len(batches[1]) != batchEntries {
		sizes := []int{}
		for _, batch := range batches {
			sizes = append(sizes, len(batch))
		}
		t.Errorf("batch sizes %v, want 256, 256, then batches the 1 MiB bound cuts", sizes)
	}
	var want []string
	for _, row := range st.rows {
		if row.Session != "scanned-newer" {
			want = append(want, row.Session)
		}
	}
	if !slices.Equal(written, want) {
		t.Errorf("wrote %d sessions, want every row but the scanned one newer than the role's registration (%d), in order", len(written), len(want))
	}
	first := batches[0][0]
	wantFirst := Entry{SessionID: "ses-0000", LegionProject: legionProject, Issue: "LEGION-7", Role: "tester",
		RoleToken: "legion-sjawharlegion-legion-7-tester", Title: "Legion tester · LEGION-7", Harness: "omp", StartedAt: "2026-10-20T09:00:00.000Z"}
	if first != wantFirst {
		t.Errorf("the first entry = %+v, want %+v", first, wantFirst)
	}
	if first.LegionProject == "legion" || first.LegionProject == "LEGION" {
		t.Errorf("legion_project %q is the Dispatch project, want the Legion project token", first.LegionProject)
	}
}

// A batch the listener refuses for one entry (a session it holds bound to another issue) is sent
// again an entry at a time: every other entry is written, and the refused one is logged and
// dropped, not sent again.
func TestARefusedBatchIsSentAgainAnEntryAtATime(t *testing.T) {
	listener, server := newStandIn(t)
	listener.conflict = "ses-1"
	st := &fakeStore{rows: []store.AgentConversation{testerRow(t, "ses-0", 0), testerRow(t, "ses-1", 1), testerRow(t, "ses-2", 2)}}
	log := &logs{}
	_, clock := runMirror(t, server, st, log)
	clock.wait(t)

	batches, _, _ := listener.snapshot()
	var written []string
	for _, batch := range batches {
		if len(batch) != 1 {
			t.Errorf("a written batch of %d entries, want each alone after the refusal", len(batch))
		}
		written = append(written, batch[0].SessionID)
	}
	if !slices.Equal(written, []string{"ses-0", "ses-2"}) {
		t.Errorf("wrote %v, want ses-0 and ses-2", written)
	}
	if lines := log.lines("refused an agent registry write", "status=409"); len(lines) != 2 {
		t.Errorf("refusal lines = %d, want the batch's and ses-1's own:\n%s", len(lines), strings.Join(log.lines(), "\n"))
	}
}

// Each registration is one PUT of its session with exactly the contract's fields: the role's
// harness, the registration's time, the title the agent gives itself. The daemon's own controller,
// which is no agent of an issue, records and writes nothing.
func TestEachRegistrationIsOnePutOfItsSession(t *testing.T) {
	listener, server := newStandIn(t)
	st := &fakeStore{}
	log := &logs{}
	m, clock := runMirror(t, server, st, log)
	clock.fire(clock.wait(t))
	clock.wait(t)

	registeredAt := clock.Now()
	recorder := Recorder{Store: st, Harness: func(claim.Role) runtime.Backend { return runtime.BackendClaude }, Now: clock.Now, Mirror: m, Log: log.logger()}
	implementer := supervise.Claim{Token: token(t, "LEGION-7", claim.RoleImplementer), Project: legionProject, Tree: "LEGION-1", Issue: "LEGION-7",
		Role: claim.RoleImplementer, Session: "4b1c7a52-0d55-4f0e-9b8e-2f1d6c3a9e10", SessionFile: "/state/claude/x/projects/-work/4b1c7a52-0d55-4f0e-9b8e-2f1d6c3a9e10.jsonl"}
	recorder.Registered(context.Background(), implementer)
	clock.wait(t)
	recorder.Registered(context.Background(), supervise.Claim{Token: claim.ControllerToken(legionProject), Project: legionProject, Role: claim.RoleController, Session: "ses-controller"})

	_, puts, _ := listener.snapshot()
	want := Entry{SessionID: implementer.Session, LegionProject: legionProject, Issue: "LEGION-7", Role: "implementer",
		RoleToken: "legion-sjawharlegion-legion-7-implementer", Title: "Legion implementer · LEGION-7", Harness: "claude",
		StartedAt: registeredAt.UTC().Format("2006-01-02T15:04:05.000Z")}
	if len(puts) != 1 || puts[0] != want {
		t.Errorf("puts = %+v, want one: %+v", puts, want)
	}
	if len(st.rows) != 1 || st.rows[0].Location != implementer.SessionFile || st.rows[0].Harness != runtime.BackendClaude {
		t.Errorf("recorded %+v, want the implementer's registration alone", st.rows)
	}
}

// A daemon whose answer loop runs writes its project's answering key saying so at boot and then
// every minute, each with the clock's time.
func TestTheHeartbeatIsWrittenEveryMinute(t *testing.T) {
	listener, server := newStandIn(t)
	_, clock := runMirror(t, server, &fakeStore{}, &logs{})
	var times []string
	for range 3 {
		times = append(times, clock.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
		w := clock.wait(t)
		if w.d != time.Minute {
			t.Fatalf("the mirror waits %s after a heartbeat, want a minute", w.d)
		}
		clock.fire(w)
	}
	clock.wait(t)
	_, _, beats := listener.snapshot()
	if len(beats) != 4 {
		t.Fatalf("heartbeats = %+v, want four: at boot and after each of three minutes", beats)
	}
	for i, at := range times {
		if beats[i] != (beat{Answering: true, UpdatedAt: at}) {
			t.Errorf("heartbeat %d = %+v, want answering at %s", i, beats[i], at)
		}
	}
}

// A daemon whose answer loop does not run says so at boot and every minute, so the listener stops
// handing its project's messages off at once rather than when the last answering key ages out. With
// `answers` set it still writes the history; without, it writes no entry at all.
func TestTheHeartbeatSaysNotAnsweringWhileTheLoopDoesNotRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store Store
		want  int
	}{
		{name: "answers without the workflow", store: &fakeStore{rows: []store.AgentConversation{testerRow(t, "ses-0", 0)}}, want: 1},
		{name: "no answers", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, server := newStandIn(t)
			_, clock := runMirrorOf(t, server, MirrorConfig{Store: tc.store, Log: (&logs{}).logger()})
			var times []string
			for range 2 {
				times = append(times, clock.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
				w := clock.wait(t)
				if w.d != time.Minute {
					t.Fatalf("the mirror waits %s after a heartbeat, want a minute", w.d)
				}
				clock.fire(w)
			}
			clock.wait(t)
			batches, puts, beats := listener.snapshot()
			if len(beats) != 3 {
				t.Fatalf("heartbeats = %+v, want three: at boot and after each of two minutes", beats)
			}
			for i, at := range times {
				if beats[i] != (beat{Answering: false, UpdatedAt: at}) {
					t.Errorf("heartbeat %d = %+v, want not answering at %s", i, beats[i], at)
				}
			}
			if len(batches) != tc.want || len(puts) != 0 {
				t.Errorf("batches %+v and puts %+v, want %d batches of the history", batches, puts, tc.want)
			}
		})
	}
}

// A listener that predates the registry answers 404 on its routes: each route's first 404 is logged
// once, every write is tried again each minute, and once the listener takes them everything held
// is written.
func TestAnOlderListenersNotFoundIsLoggedOnceAndRetriedEachMinute(t *testing.T) {
	listener, server := newStandIn(t)
	for _, route := range []string{"batch", "project"} {
		listener.setStatus(route, http.StatusNotFound)
	}
	st := &fakeStore{rows: []store.AgentConversation{testerRow(t, "ses-0", 0)}}
	log := &logs{}
	_, clock := runMirror(t, server, st, log)
	for range 3 {
		w := clock.wait(t)
		if w.d != time.Minute {
			t.Fatalf("the mirror waits %s after a 404, want a minute", w.d)
		}
		clock.fire(w)
	}
	w := clock.wait(t)
	for _, route := range []string{"PUT /v1/legion/agents:", "PUT /v1/legion/projects/{project}:"} {
		if lines := log.lines("answers 404 on " + route); len(lines) != 1 {
			t.Errorf("404 lines for %s after four tries = %d, want one:\n%s", route, len(lines), strings.Join(log.lines(), "\n"))
		}
	}
	if batches, _, beats := listener.snapshot(); len(batches) != 0 || len(beats) != 0 {
		t.Fatalf("the stand-in took %+v and %+v while it answered 404", batches, beats)
	}

	for _, route := range []string{"batch", "project"} {
		listener.setStatus(route, 0)
	}
	clock.fire(w)
	clock.wait(t)
	batches, _, beats := listener.snapshot()
	if len(batches) != 1 || batches[0][0].SessionID != "ses-0" || len(beats) != 1 {
		t.Errorf("after the listener took the routes: batches %+v, beats %+v; want the history and a heartbeat", batches, beats)
	}
}

// Any other failure is tried again on a backoff shorter than the minute a 404 waits, and the
// heartbeat keeps its own minute meanwhile.
func TestAnyOtherFailureIsRetriedOnABackoff(t *testing.T) {
	listener, server := newStandIn(t)
	listener.setStatus("batch", http.StatusServiceUnavailable)
	st := &fakeStore{rows: []store.AgentConversation{testerRow(t, "ses-0", 0)}}
	_, clock := runMirror(t, server, st, &logs{})
	w := clock.wait(t)
	if w.d < time.Second || w.d > 2*time.Second {
		t.Fatalf("the mirror waits %s after a 503, want the backoff's first step", w.d)
	}
	clock.fire(w)
	w = clock.wait(t)
	if w.d < 2*time.Second || w.d > 4*time.Second {
		t.Fatalf("the mirror waits %s after a second 503, want the backoff's second step", w.d)
	}
	listener.setStatus("batch", 0)
	clock.fire(w)
	w = clock.wait(t)
	batches, _, beats := listener.snapshot()
	if len(batches) != 1 || len(beats) != 1 {
		t.Errorf("batches %+v and heartbeats %+v, want the history written once the listener recovered, and one heartbeat within the minute", batches, beats)
	}
	if w.d <= 50*time.Second {
		t.Errorf("the mirror waits %s with everything written, want the rest of the heartbeat's minute", w.d)
	}
}

// fireUntil fires the waits the mirror asks for until holds, so a test does not have to count
// the passes a queued write's own wake adds.
func fireUntil(t *testing.T, clock *fakeClock, what string, holds func() bool) {
	t.Helper()
	for range 20 {
		if holds() {
			return
		}
		clock.fire(clock.wait(t))
	}
	t.Fatalf("the mirror never %s", what)
}

// An entry's `finished` is the standing of its issue: false while the issue runs, so the listener
// keeps a message to that role for its next worker (INFRA-514's 202), and true once the issue has
// left the workflow - closed, parked, or its tree retired - so the listener hands such a message
// to this daemon, which answers it from the stopped agent's own conversation. The mirror writes
// every conversation of an issue again whenever that standing changes, in either direction.
func TestTheMirrorWritesFinishedWhenAnIssueLeavesTheWorkflowAndAgainWhenItReturns(t *testing.T) {
	listener, server := newStandIn(t)
	st := &fakeStore{rows: []store.AgentConversation{testerRow(t, "ses-0", 0), testerRow(t, "ses-1", 1)}}
	_, clock := runMirror(t, server, st, &logs{})
	w := clock.wait(t)

	batches, _, _ := listener.snapshot()
	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("the boot wrote %+v, want one batch holding both conversations", batches)
	}
	for _, entry := range batches[0] {
		if entry.Finished {
			t.Fatalf("the boot wrote %s finished while its issue runs: a message to its role would be answered from a stopped agent instead of kept for the role's next worker", entry.SessionID)
		}
	}

	// The issue leaves the workflow: every conversation of it is written again, finished.
	st.setFinished("LEGION-7", true)
	clock.fire(w)
	fireUntil(t, clock, "wrote the closed issue's conversations again", func() bool {
		_, puts, _ := listener.snapshot()
		return len(puts) == 2
	})
	_, puts, _ := listener.snapshot()
	if puts[0].SessionID != "ses-0" || puts[1].SessionID != "ses-1" || !puts[0].Finished || !puts[1].Finished {
		t.Fatalf("the close wrote %+v, want both conversations, oldest first, each finished", puts)
	}

	// A standing that did not change writes nothing, whatever the minute brings.
	for range 2 {
		clock.fire(clock.wait(t))
	}
	if _, again, _ := listener.snapshot(); len(again) != 2 {
		t.Fatalf("a standing that did not change wrote %+v, want only the two writes the close made", again)
	}

	// And back into the workflow - a person moved the issue in again: the entries say so, so the
	// role's next worker is kept its messages once more.
	st.setFinished("LEGION-7", false)
	fireUntil(t, clock, "wrote the reopened issue's conversations again", func() bool {
		_, puts, _ := listener.snapshot()
		return len(puts) == 4
	})
	_, puts, _ = listener.snapshot()
	if puts[2].Finished || puts[3].Finished {
		t.Fatalf("the reopen wrote %+v, want both conversations again with finished false", puts[2:])
	}
}

// A registration recorded while its issue is finished carries that standing, and a standing read
// that fails changes nothing and is read again on the lane's backoff.
func TestTheMirrorWritesTheStandingItLastReadAndRetriesAReadThatFailed(t *testing.T) {
	listener, server := newStandIn(t)
	st := &fakeStore{rows: []store.AgentConversation{testerRow(t, "ses-history", 0)}}
	st.setFinished("LEGION-7", true)
	st.failStandings(fmt.Errorf("the record is not readable"))
	log := &logs{}
	m, clock := runMirror(t, server, st, log)
	fireUntil(t, clock, "wrote the history", func() bool {
		batches, _, _ := listener.snapshot()
		return len(batches) == 1
	})
	if batches, _, _ := listener.snapshot(); batches[0][0].Finished {
		t.Fatalf("the boot wrote %+v while no standing could be read, want every entry unfinished: the keep is what an unread standing leaves", batches[0][0])
	}

	// A registration made while no standing could be read is unfinished too, which is the answer
	// every entry had before this field.
	m.Put(testerRow(t, "ses-registered", 1))
	fireUntil(t, clock, "wrote the registration", func() bool {
		_, puts, _ := listener.snapshot()
		return len(puts) == 1
	})
	if _, puts, _ := listener.snapshot(); puts[0].Finished {
		t.Fatalf("a registration written while no standing could be read is %+v, want it unfinished", puts[0])
	}
	if lines := log.lines("could not read the standing of its issues"); len(lines) == 0 {
		t.Fatalf("the failed standing read was not logged:\n%s", strings.Join(log.lines(), "\n"))
	}

	// The read recovers: the finished issue's conversation is written again, finished, and a
	// registration made after it carries the standing too.
	st.failStandings(nil)
	fireUntil(t, clock, "wrote the finished issue's conversation again", func() bool {
		_, puts, _ := listener.snapshot()
		return len(puts) == 2
	})
	if _, puts, _ := listener.snapshot(); !puts[1].Finished || puts[1].SessionID != "ses-history" {
		t.Fatalf("the recovered read wrote %+v, want the finished issue's recorded conversation written again", puts[1])
	}
	m.Put(testerRow(t, "ses-later", 2))
	fireUntil(t, clock, "wrote the second registration", func() bool {
		_, puts, _ := listener.snapshot()
		return len(puts) == 3
	})
	if _, puts, _ := listener.snapshot(); !puts[2].Finished {
		t.Fatalf("a registration of a finished issue is %+v, want it finished", puts[2])
	}
}
