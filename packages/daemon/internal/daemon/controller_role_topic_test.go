package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// legionController is project legion's controller role (claim.ControllerToken), and
// controllerRoleTopic its role topic, notifications.role.legion-<project>-controller
// (notify.RoleTopicPrefix and claim.ControllerToken), written out so that a change of either is a
// change these tests read.
const (
	legionController    = "legion-legion-controller"
	controllerRoleTopic = "notifications.role." + legionController
)

// R2 (d) of dispatch://INFRA-1585: a controller notice row waits while no session holds the
// controller role, and while the registered controller session holds it but the listener reads it
// unresponsive, and logs each attempt as a wait, never as a failed row; once the registered
// session is the role's live holder, the row is published once, on the controller's role topic,
// without keep, under the row's own key, and finishes.
//
// The control is the code before this change (the pull request records the run): there the first
// attempt publishes the notice to the controller topic (notifications.legion.legion.controller)
// while no session holds the role, which finishes the row, so the test stops at its first
// assertion. What that code does with an unresponsive holder's answer is the control of
// TestAControllerNoticeWhoseHolderLapsesBeforeThePublishWaits.
func TestAControllerNoticeWaitsForALiveHolderOfTheControllerRoleThenGoesToItsRoleTopicOnce(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	registerProjectControllerAs(t, pool.Config().ConnString(), "legion", "ses_controller")
	listener := newControllerRoleListener()
	server := httptest.NewServer(listener)
	t.Cleanup(server.Close)
	clock := time.Now().UTC()
	row := mustOutboxRow(t, "LEGION-2", record.ControllerNotice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning}, clock)
	enqueueOutbox(t, pool, records, row)
	// The row's own key names the id the store gave it, which the row built above does not hold.
	row.ID = noticeRowID(t, pool)
	// At debug, since settle logs a row's second wait there (the wait's first attempt is at INFO).
	var logged syncBuffer
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})), pool: pool, dispatchProject: "LEGION", records: records,
		notices: notify.New(server.URL, ""), project: "legion", now: func() time.Time { return clock }}
	attempt := func(what string) {
		t.Helper()
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("run the outbox %s: %v", what, err)
		}
	}

	attempt("while no session holds the controller role")
	if got := listener.publishedTopics(); len(got) != 0 {
		t.Fatalf("published to %v while no session holds the controller role, want nothing: the notice waits for a live holder", got)
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("%d outbox rows after the attempt with no holder, want the notice's row kept", rows)
	}

	listener.answer("ses_controller", notify.StateUnresponsive)
	clock = clock.Add(record.RoleWaitBackoffMost + time.Second)
	attempt("while the registered controller session holds the role unresponsive")
	if got := listener.publishedTopics(); len(got) != 0 {
		t.Fatalf("published to %v while the controller role's holder is unresponsive, want nothing", got)
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("%d outbox rows after the attempt with an unresponsive holder, want the notice's row kept", rows)
	}

	listener.answer("ses_controller", notify.StateLive)
	clock = clock.Add(record.RoleWaitBackoffMost + time.Second)
	attempt("once the registered controller session holds the role live")
	published := listener.publishes()
	if len(published) != 1 {
		t.Fatalf("published %+v once the registered session holds the role live, want the notice once", published)
	}
	if p := published[0]; p.Topic != controllerRoleTopic || p.Message != "held on LEGION-2" || p.DedupeKey != record.OutboxKey(row.ID) ||
		p.Keep == nil || *p.Keep {
		keep := "absent"
		if p.Keep != nil {
			keep = fmt.Sprint(*p.Keep)
		}
		t.Fatalf("published %+v (keep %s), want held on LEGION-2 on %s under %s with keep false", p, keep, controllerRoleTopic, record.OutboxKey(row.ID))
	}
	if rows := outboxRows(t, pool); rows != 0 {
		t.Fatalf("%d outbox rows once the notice was published, want none", rows)
	}

	text := logged.String()
	if strings.Contains(text, "level=ERROR") || strings.Contains(text, `msg="outbox row failed"`) {
		t.Fatalf("the two waits were logged as a failure:\n%s", text)
	}
	var waits []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, `msg="outbox row waits"`) && strings.Contains(line, "kind=controller_notice") {
			waits = append(waits, line)
		}
	}
	if len(waits) != 2 || !strings.Contains(waits[0], "no holder") ||
		!strings.Contains(waits[1], "ses_controller") || !strings.Contains(waits[1], notify.StateUnresponsive) {
		t.Fatalf("logged the waits %q, want two: one naming no holder, then one naming ses_controller unresponsive:\n%s", waits, text)
	}
}

// The publish-time half of R2 (d): the listener names the registered controller session the
// role's live holder when the outbox reads the role, and the holder's live window ends before the
// publish, so the publish is answered as the listener answers a lapsed holder it reads
// unresponsive (a 404 naming holder_state unresponsive, which stores nothing). The row waits,
// the attempt is logged as a wait naming the unresponsive holder, and nothing fails.
//
// The control is the code before this change (the pull request records the run): there
// controllerNotice waits on notify.ErrNoHolder alone, so the same answer to its publish fails the
// attempt as an error, and settle logs "outbox row failed" at ERROR. The real listener gives that
// answer on a role topic only; this fake gives it to every publish, that code's publish to the
// controller topic among them, so that a run of that code reads its own handling of the answer.
func TestAControllerNoticeWhoseHolderLapsesBeforeThePublishWaits(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	registerProjectControllerAs(t, pool.Config().ConnString(), "legion", "ses_controller")
	listener := newControllerRoleListener()
	listener.answer("ses_controller", notify.StateLive)
	listener.lapseAtPublish(notify.StateUnresponsive)
	server := httptest.NewServer(listener)
	t.Cleanup(server.Close)
	clock := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.ControllerNotice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning}, clock))
	var logged syncBuffer
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records,
		notices: notify.New(server.URL, ""), project: "legion", now: func() time.Time { return clock }}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}
	if attempted := listener.attemptedTopics(); len(attempted) != 1 {
		t.Fatalf("the outbox asked the listener to publish to %v, want one publish: the holder read live, so the attempt reaches the publish", attempted)
	}
	if got := listener.publishedTopics(); len(got) != 0 {
		t.Fatalf("the listener took publishes to %v, want none: it answers every publish as an unresponsive holder", got)
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("%d outbox rows, want the notice's row kept", rows)
	}
	text := logged.String()
	if strings.Contains(text, "level=ERROR") || strings.Contains(text, `msg="outbox row failed"`) {
		t.Fatalf("the unresponsive holder's answer to the publish to %v failed the attempt as an error:\n%s", listener.attemptedTopics(), text)
	}
	if !strings.Contains(text, `msg="outbox row waits"`) || !strings.Contains(text, notify.StateUnresponsive) {
		t.Fatalf("logged %q, want one wait naming the unresponsive holder", text)
	}
}

// R2 (e): every kind of controller notice goes to the controller's role topic, a project's tick
// (keyed by the Dispatch project alone) and a stopped tree's notice among them, each under its
// own row's key.
//
// The control is the code before this change: there both go to the controller topic.
func TestATickAndATreeStalledNoticeGoToTheControllersRoleTopic(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	registerProjectControllerAs(t, pool.Config().ConnString(), "legion", "ses_controller")
	listener := newControllerRoleListener()
	listener.answer("ses_controller", notify.StateLive)
	server := httptest.NewServer(listener)
	t.Cleanup(server.Close)
	clock := time.Now().UTC()
	var keys []string
	for _, row := range []record.OutboxRow{
		mustOutboxRow(t, "LEGION", record.ControllerNotice{Kind: record.TickNotice}, clock),
		mustOutboxRow(t, "LEGION-1", record.ControllerNotice{Kind: record.TreeStalledNotice, Reason: "the tree of LEGION-1 has not moved"}, clock),
	} {
		enqueueOutbox(t, pool, records, row)
		var id int64
		if err := pool.QueryRow(context.Background(), "select id from outbox where issue = $1", row.Issue).Scan(&id); err != nil {
			t.Fatalf("read the id of %s's row: %v", row.Issue, err)
		}
		keys = append(keys, row.Issue+" "+record.OutboxKey(id))
	}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
		notices: notify.New(server.URL, ""), project: "legion", now: func() time.Time { return clock }}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}
	var messages, published []string
	for _, p := range listener.publishes() {
		if p.Topic != controllerRoleTopic {
			t.Fatalf("published %q to %s, want every controller notice on %s", p.Message, p.Topic, controllerRoleTopic)
		}
		messages = append(messages, p.Message)
		_, issue, _ := strings.Cut(p.Message, " on ")
		published = append(published, issue+" "+p.DedupeKey)
	}
	slices.Sort(messages)
	if want := []string{"tick on LEGION", "tree-stalled on LEGION-1"}; !slices.Equal(messages, want) {
		t.Fatalf("published %v, want %v on %s", messages, want, controllerRoleTopic)
	}
	slices.Sort(published)
	if slices.Sort(keys); !slices.Equal(published, keys) {
		t.Fatalf("published under the keys %v, want each notice under its own row's key, %v", published, keys)
	}
	if rows := outboxRows(t, pool); rows != 0 {
		t.Fatalf("%d outbox rows once both were published, want none", rows)
	}
}

// The registered-session half of controllerHolds: a controller notice waits while the live holder
// of the controller role is a session other than the one the daemon records as its registered
// controller, logs the wait naming both sessions, and publishes nothing. A session that holds the
// role but is not the registered controller (one an older `legion controller start` left, or one
// the operator started beside it) is not the controller the notice is for.
//
// The control (the pull request records the run): controllerHolds reading the state alone, `if
// state != notify.StateLive`, publishes the notice to the other session.
func TestAControllerNoticeWaitsWhileTheRolesLiveHolderIsNotTheRegisteredSession(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	registerProjectControllerAs(t, pool.Config().ConnString(), "legion", "ses_controller")
	listener := newControllerRoleListener()
	listener.answer("ses_other", notify.StateLive)
	server := httptest.NewServer(listener)
	t.Cleanup(server.Close)
	clock := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.ControllerNotice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning}, clock))
	var logged syncBuffer
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records,
		notices: notify.New(server.URL, ""), project: "legion", now: func() time.Time { return clock }}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}
	if got := listener.publishedTopics(); len(got) != 0 {
		t.Fatalf("published to %v while the role's live holder is ses_other and the registered controller ses_controller, want nothing", got)
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("%d outbox rows, want the notice's row kept", rows)
	}
	text := logged.String()
	if strings.Contains(text, "level=ERROR") || !strings.Contains(text, `msg="outbox row waits"`) ||
		!strings.Contains(text, "ses_controller") || !strings.Contains(text, "ses_other") {
		t.Fatalf("logged %q, want one wait naming the registered session ses_controller and the holder ses_other, and no error", text)
	}
}

// The no-registration half of controllerHolds: with no controller registered, a controller notice
// waits, publishes nothing and keeps its row, and its wait is logged by the wait rather than by
// the row's attempts (dispatch://INFRA-4131/spec#b-r-logged-once), so a wait under way stays at
// DEBUG past waitWarnAttempts: the liveness sweep already says no controller is registered
// (controllerNotRegistered), and each waiting row would otherwise repeat it every minute. The
// listener names a live holder, so a wait the registration did not decide would publish.
//
// The control: logging the wait by its attempts, as settle logs a wait of a kind that does not
// wait for its role (waitWarnAttempts), writes this run at WARN.
func TestAControllerNoticeWithNoControllerRegisteredWaitsAtDebug(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	listener := newControllerRoleListener()
	listener.answer("ses_controller", notify.StateLive)
	server := httptest.NewServer(listener)
	t.Cleanup(server.Close)
	clock := time.Now().UTC()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.ControllerNotice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning}, clock))
	// A wait under way: the row has run waitWarnAttempts times, the first of them ten minutes ago,
	// which started its wait and logged that start at INFO.
	if _, err := pool.Exec(context.Background(), "update outbox set attempts = $1, wait_step = $1, wait_since = $2",
		waitWarnAttempts, clock.Add(-10*time.Minute)); err != nil {
		t.Fatalf("age the row to attempt %d of a wait under way: %v", waitWarnAttempts, err)
	}
	var logged syncBuffer
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})), pool: pool,
		dispatchProject: "LEGION", records: records, notices: notify.New(server.URL, ""), project: "legion", now: func() time.Time { return clock }}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}
	if got := listener.publishedTopics(); len(got) != 0 {
		t.Fatalf("published to %v with no controller registered, want nothing", got)
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("%d outbox rows, want the notice's row kept", rows)
	}
	var waits []string
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, `msg="outbox row waits"`) {
			waits = append(waits, line)
		}
	}
	if len(waits) != 1 || !strings.Contains(waits[0], "level=DEBUG") || !strings.Contains(waits[0], "waits for a controller to register") {
		t.Fatalf("logged the waits %q, want one at DEBUG naming the missing registration at attempt %d", waits, waitWarnAttempts)
	}
}

// controllerRoleListener is the Envoy listener's two routes as a controller notice meets them:
// the role lookup (GET /v1/roles/<role>, as roleGetHandler and writeRoleHolderError answer it)
// and the publish (POST /v1/messages/publish). It names one holder of every role, in one state:
// none at first, which is the 404 of a role no claim names; `live`, which is the 200; or any
// other state, which is the 404 of a lapsed holder in that state. A publish to a role topic is
// answered as the listener answers keep: false, by that same holder: a live one takes it with
// 200, and every other answer is the lookup's 404, which stores nothing. A publish to any other
// topic is taken. Once a test calls lapseAtPublish, every publish, to any topic, is answered as
// the 404 of a lapsed holder in that state, whatever the lookup says. Every publish asked for is
// recorded (attemptedTopics), and every publish taken (publishes).
type controllerRoleListener struct {
	mu        sync.Mutex
	holder    string
	state     string
	lapse     string
	attempted []string
	published []controllerRolePublish
}

type controllerRolePublish struct {
	Topic     string `json:"topic"`
	Message   string `json:"message"`
	DedupeKey string `json:"dedupe_key"`
	Keep      *bool  `json:"keep"`
}

func newControllerRoleListener() *controllerRoleListener { return &controllerRoleListener{} }

// answer is the holder every role lookup and role publish names from now on.
func (l *controllerRoleListener) answer(holder, state string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holder, l.state = holder, state
}

// lapseAtPublish makes every publish from now on be answered as a lapsed holder in state: the
// answer of a holder whose live window ended between the outbox's read of the role and its
// publish.
func (l *controllerRoleListener) lapseAtPublish(state string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lapse = state
}

// attemptedTopics is the topic of every publish asked for, taken or not.
func (l *controllerRoleListener) attemptedTopics() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.attempted)
}

func (l *controllerRoleListener) publishes() []controllerRolePublish {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.published)
}

func (l *controllerRoleListener) publishedTopics() []string {
	var topics []string
	for _, p := range l.publishes() {
		topics = append(topics, p.Topic)
	}
	return topics
}

func (l *controllerRoleListener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if role, ok := strings.CutPrefix(r.URL.Path, "/v1/roles/"); ok && r.Method == http.MethodGet {
		l.writeHolder(w, role)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/publish" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
		return
	}
	body, err := io.ReadAll(r.Body)
	var published controllerRolePublish
	if err != nil || json.Unmarshal(body, &published) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	l.attempted = append(l.attempted, published.Topic)
	if l.lapse != "" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"error":"holder %s lapsed; claim retained","reason":"holder_lapsed","holder":%q,"holder_state":%q,"last_seen":1,"claim_released":false}`,
			l.holder, l.holder, l.lapse)
		return
	}
	if role, ok := strings.CutPrefix(published.Topic, notify.RoleTopicPrefix); ok && (l.holder == "" || l.state != notify.StateLive) {
		l.writeHolder(w, role)
		return
	}
	l.published = append(l.published, published)
	_, _ = io.WriteString(w, `{}`)
}

// writeHolder is the role lookup's answer for role, by the holder the listener names.
func (l *controllerRoleListener) writeHolder(w http.ResponseWriter, role string) {
	switch {
	case l.holder == "":
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"error":"role %s is unclaimed","reason":"unclaimed"}`, role)
	case l.state == notify.StateLive:
		var body bytes.Buffer
		_ = json.NewEncoder(&body).Encode(map[string]any{"role": role, "holder": l.holder, "last_seen": time.Now().UnixMilli(),
			"holder_state": "live", "capabilities": []string{}})
		_, _ = w.Write(body.Bytes())
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"error":"role %s holder %s lapsed; claim retained","reason":"holder_lapsed","holder":%q,"holder_state":%q,"last_seen":1,"claim_released":false}`,
			role, l.holder, l.holder, l.state)
	}
}
